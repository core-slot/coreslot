//go:build unix

package docker_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/core-slot/coreslot/internal/docker"
)

// files is a directory of stand-in sockets: ref stands for Docker Desktop's own socket, other for
// another engine's, link is a symlink to ref, gone names nothing.
type files struct{ ref, other, link, gone string }

func newFiles(t *testing.T) files {
	t.Helper()
	dir := t.TempDir()
	f := files{
		ref:   filepath.Join(dir, "docker.sock"),
		other: filepath.Join(dir, "other.sock"),
		link:  filepath.Join(dir, "link.sock"),
		gone:  filepath.Join(dir, "gone.sock"),
	}
	for _, p := range []string{f.ref, f.other} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(f.ref, f.link); err != nil {
		t.Fatal(err)
	}
	return f
}

// good is the input of a run that engages: features on, interest, Docker Desktop's own socket.
func good(f files) docker.EngageInput {
	return docker.EngageInput{Interest: true, Reference: f.ref}
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// engage runs Engage and checks its two results.
func engage(t *testing.T, name string, in docker.EngageInput, wantUpstream, wantNote string) {
	t.Helper()
	up, note := docker.Engage(in)
	if up != wantUpstream || note != wantNote {
		t.Errorf("%s: Engage = %q, %q; want %q, %q", name, up, note, wantUpstream, wantNote)
	}
}

// The good input engages on Docker Desktop's socket with no note; the baseline the other tests
// change one field of. Mutation: engage only under DOCKER_HOST or docker_forward = true → no
// upstream, red.
func TestEngageGood(t *testing.T) {
	f := newFiles(t)
	engage(t, "good", good(f), f.ref, "")
}

// Every reason the features are off is the run's note when the run has a Docker interest, and
// nothing without one; the platform is never said. Mutations: give the note without interest →
// the no-interest rows, red; say OffPlatform → its row, red.
func TestEngageOffReasons(t *testing.T) {
	f := newFiles(t)
	for _, off := range []docker.Off{docker.OffConfig, docker.OffNested, docker.OffStateDir, docker.OffRemoteHost} {
		in := good(f)
		in.Off = off
		engage(t, string(off)+" with interest", in, "", "no-forwarder: "+string(off))
		in.Interest = false
		engage(t, string(off)+" without interest", in, "", "")
	}
	in := good(f)
	in.Off = docker.OffPlatform
	engage(t, "platform", in, "", "")
	if got := docker.OffNote(docker.OffConfig); got != "no-forwarder: config" {
		t.Errorf("OffNote(config) = %q", got)
	}
}

// An unreadable needs entry never turns into "on", not even when it may have said
// docker_forward = true. Mutation: treat a bad entry as absent → engaged, red.
func TestEngageBadEntryNeverOn(t *testing.T) {
	f := newFiles(t)
	in := good(f)
	in.EntryBad = true
	engage(t, "bad entry", in, "", docker.NoteNeedsEntry)
	in.Interest = false
	engage(t, "bad entry without interest", in, "", "")
}

// docker_forward = false wins over every interest. Mutation: let interest win → engaged, red.
func TestEngageSwitchFalseWins(t *testing.T) {
	f := newFiles(t)
	in := good(f)
	in.Switch = no()
	engage(t, "switch false", in, "", docker.NoteSwitch)
}

// docker_forward = true is an interest by itself. Mutation: require Interest as well → no
// upstream, red.
func TestEngageSwitchTrueWithoutHistory(t *testing.T) {
	f := newFiles(t)
	in := good(f)
	in.Interest, in.Switch = false, yes()
	engage(t, "switch true", in, f.ref, "")
}

// A DOCKER_HOST that names Docker Desktop's socket through a symlink is the same file by device
// and inode: engaged, with the user's own path as the upstream. Mutation: compare the strings →
// NoteUpstream, red.
func TestEngageSameFileByInode(t *testing.T) {
	f := newFiles(t)
	in := good(f)
	in.UserHost = "unix://" + f.link
	engage(t, "symlinked DOCKER_HOST", in, f.link, "")
	if !docker.SameFile(f.link, f.ref) || docker.SameFile(f.other, f.ref) || docker.SameFile(f.gone, f.gone) {
		t.Error("SameFile: want a symlink the same file, another file not, a missing file never")
	}
}

// A DOCKER_HOST naming another socket is not Docker Desktop's: no forwarder. Mutation: skip
// condition 3 for a user host → engaged, red.
func TestEngageOtherSocketIsUpstreamNote(t *testing.T) {
	f := newFiles(t)
	in := good(f)
	in.UserHost = "unix://" + f.other
	engage(t, "another socket", in, "", docker.NoteUpstream)
	in.UserHost = "tcp://192.0.2.10:2376"
	engage(t, "a tcp host", in, "", docker.NoteUpstream)
}

// Docker quit: the reference socket is gone, so nothing is the same file as it. Mutation: let a
// missing pair compare equal → engaged, red.
func TestEngageNoReferenceSocket(t *testing.T) {
	f := newFiles(t)
	in := good(f)
	in.Reference = f.gone
	engage(t, "no reference", in, "", docker.NoteUpstream)
	in.UserHost = "unix://" + f.gone
	engage(t, "no reference, DOCKER_HOST naming it", in, "", docker.NoteUpstream)
}

// A run without a Docker interest says nothing. Mutation: engage without
// interest → upstream, red.
func TestEngageNoInterestSilent(t *testing.T) {
	f := newFiles(t)
	in := good(f)
	in.Interest = false
	engage(t, "no interest", in, "", "")
}

// Every note fits the hold's docker_note: at most 64 bytes of [a-z0-9:. -].
func TestNoteWords(t *testing.T) {
	notes := []string{docker.NoteNeedsEntry, docker.NoteSwitch, docker.NoteUpstream, docker.NoteAddress,
		docker.NoteStart, docker.NoteBlind, docker.NoteNoReading, docker.NoteOversized}
	for _, off := range []docker.Off{docker.OffConfig, docker.OffNested, docker.OffStateDir, docker.OffRemoteHost} {
		notes = append(notes, docker.OffNote(off))
	}
	seen := map[string]bool{}
	for _, n := range notes {
		if seen[n] || len(n) == 0 || len(n) > 64 {
			t.Errorf("note %q: duplicate, empty or over 64 bytes", n)
		}
		seen[n] = true
		for _, c := range []byte(n) {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == ':' || c == '.' || c == ' ' || c == '-') {
				t.Errorf("note %q has %q", n, c)
			}
		}
	}
}
