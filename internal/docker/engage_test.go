//go:build unix

package docker_test

import (
	"context"
	"errors"
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

// asker is an Ask that answers a and counts its calls.
type asker struct {
	a     docker.Answers
	err   error
	calls int
}

func (k *asker) ask(context.Context) (docker.Answers, error) {
	k.calls++
	return k.a, k.err
}

// good is the input of a run that engages with the questions asked: features on, interest, Docker
// Desktop's own socket at every place the command could reach.
func good(f files, k *asker) docker.EngageInput {
	k.a = docker.Answers{Endpoints: []string{"unix://" + f.link, "unix://" + f.ref}, BuilderDriver: "docker"}
	return docker.EngageInput{Interest: true, Reference: f.ref, LibrarySocket: f.link, Ask: k.ask}
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// engage runs Engage and checks its two results.
func engage(t *testing.T, name string, in docker.EngageInput, wantUpstream, wantNote string) {
	t.Helper()
	up, note := docker.Engage(context.Background(), in)
	if up != wantUpstream || note != wantNote {
		t.Errorf("%s: Engage = %q, %q; want %q, %q", name, up, note, wantUpstream, wantNote)
	}
}

// The good input engages on Docker Desktop's socket with no note; the baseline the other tests
// change one field of.
func TestEngageGood(t *testing.T) {
	f, k := newFiles(t), &asker{}
	engage(t, "good", good(f, k), f.ref, "")
	if k.calls != 1 {
		t.Errorf("Ask called %d times, want once", k.calls)
	}
}

// Every reason the features are off is the run's note when the run has a Docker interest, and
// nothing without one; the platform is never said. Mutations: give the note without interest →
// the no-interest rows, red; say OffPlatform → its row, red.
func TestEngageOffReasons(t *testing.T) {
	f := newFiles(t)
	for _, off := range []docker.Off{docker.OffConfig, docker.OffNested, docker.OffStateDir, docker.OffRemoteHost} {
		k := &asker{}
		in := good(f, k)
		in.Off = off
		engage(t, string(off)+" with interest", in, "", "no-forwarder: "+string(off))
		in.Interest = false
		engage(t, string(off)+" without interest", in, "", "")
		if k.calls != 0 {
			t.Errorf("%s: Ask called %d times", off, k.calls)
		}
	}
	in := good(f, &asker{})
	in.Off = docker.OffPlatform
	engage(t, "platform", in, "", "")
	if got := docker.OffNote(docker.OffConfig); got != "no-forwarder: config" {
		t.Errorf("OffNote(config) = %q", got)
	}
}

// A run started by coreslot's own questions takes no forwarder and asks nothing, which ends a
// wrapper docker's recursion. Mutation: drop the probe case → engaged, red.
func TestEngageProbe(t *testing.T) {
	f, k := newFiles(t), &asker{}
	in := good(f, k)
	in.Probe = true
	engage(t, "probe", in, "", docker.NoteProbe)
	in.Switch = yes()
	engage(t, "probe with the switch on", in, "", docker.NoteProbe)
	if k.calls != 0 {
		t.Errorf("Ask called %d times for a probe", k.calls)
	}
}

// An unreadable needs entry never turns into "on", not even when it may have said
// docker_forward = true. Mutation: treat a bad entry as absent → engaged, red.
func TestEngageBadEntryNeverOn(t *testing.T) {
	f := newFiles(t)
	in := good(f, &asker{})
	in.EntryBad = true
	engage(t, "bad entry", in, "", docker.NoteNeedsEntry)
	in.Interest = false
	engage(t, "bad entry without interest", in, "", "")
}

// docker_forward = false wins over every interest. Mutation: let interest win → engaged, red.
func TestEngageSwitchFalseWins(t *testing.T) {
	f := newFiles(t)
	in := good(f, &asker{})
	in.Switch = no()
	engage(t, "switch false", in, "", docker.NoteSwitch)
}

// docker_forward = true is an interest by itself and skips condition 5: a remote context and
// another builder do not withhold the forwarder, and nothing is asked. Mutations: require
// Interest as well → no upstream, red; ask anyway → Ask called, red.
func TestEngageSwitchTrueWithoutHistory(t *testing.T) {
	f, k := newFiles(t), &asker{}
	in := good(f, k)
	in.Interest, in.Switch = false, yes()
	k.a = docker.Answers{Endpoints: []string{"tcp://192.0.2.10:2376"}, BuilderDriver: "docker-container"}
	in.LibrarySocket = f.other
	engage(t, "switch true", in, f.ref, "")
	if k.calls != 0 {
		t.Errorf("Ask called %d times under docker_forward = true", k.calls)
	}
}

// A DOCKER_HOST that names Docker Desktop's socket through a symlink is the same file by device
// and inode: engaged, with the user's own path as the upstream. Mutation: compare the strings →
// NoteUpstream, red.
func TestEngageSameFileByInode(t *testing.T) {
	f, k := newFiles(t), &asker{}
	in := good(f, k)
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
	in := good(f, &asker{})
	in.UserHost = "unix://" + f.other
	engage(t, "another socket", in, "", docker.NoteUpstream)
	in.UserHost = "tcp://192.0.2.10:2376"
	engage(t, "a tcp host", in, "", docker.NoteUpstream)
}

// Docker quit: the reference socket is gone, so nothing is the same file as it. Mutation: let a
// missing pair compare equal → engaged, red.
func TestEngageNoReferenceSocket(t *testing.T) {
	f, k := newFiles(t), &asker{}
	in := good(f, k)
	in.Reference = f.gone
	engage(t, "no reference", in, "", docker.NoteUpstream)
	in.UserHost = "unix://" + f.gone
	engage(t, "no reference, DOCKER_HOST naming it", in, "", docker.NoteUpstream)
	if k.calls != 0 {
		t.Errorf("Ask called %d times with Docker gone", k.calls)
	}
}

// A context with a remote endpoint is something the command could switch to. Mutation: check only
// the current context → engaged, red (the remote one is listed first).
func TestEngageRemoteContext(t *testing.T) {
	f, k := newFiles(t), &asker{}
	in := good(f, k)
	k.a.Endpoints = append([]string{"tcp://192.0.2.10:2376"}, k.a.Endpoints...)
	engage(t, "remote context", in, "", docker.NoteRemoteContext)
}

// A context naming a socket that does not exist counts as another endpoint: the strict form.
// Mutation: skip endpoints whose file is missing → engaged, red.
func TestEngageMissingContextSocket(t *testing.T) {
	f, k := newFiles(t), &asker{}
	in := good(f, k)
	k.a.Endpoints = append(k.a.Endpoints, "unix://"+f.gone)
	engage(t, "missing context socket", in, "", docker.NoteRemoteContext)
}

// The socket libraries go to must be Docker Desktop's too, and a missing one is not: the case of
// Docker Desktop's default socket switched off. Mutation: drop the library socket check → both
// rows engaged, red.
func TestEngageVarRunSockMustBeSameFile(t *testing.T) {
	f := newFiles(t)
	for name, lib := range map[string]string{"another engine": f.other, "missing": f.gone} {
		in := good(f, &asker{})
		in.LibrarySocket = lib
		engage(t, name, in, "", docker.NoteRemoteContext)
	}
}

// A selected builder whose driver is not docker would build elsewhere; an empty driver (a context
// that cannot be reached) is not docker either. Mutation: accept any driver → engaged, red.
func TestEngageBuilderDriver(t *testing.T) {
	f := newFiles(t)
	for _, driver := range []string{"docker-container", ""} {
		k := &asker{}
		in := good(f, k)
		k.a.BuilderDriver = driver
		engage(t, "driver "+driver, in, "", docker.NoteBuilder)
	}
}

// No docker, a failed question or a missed deadline: not engaged. Mutation: ignore Ask's error →
// engaged, red.
func TestEngageAskFails(t *testing.T) {
	f, k := newFiles(t), &asker{}
	in := good(f, k)
	k.err = errors.New("exec: \"docker\": executable file not found in $PATH")
	engage(t, "ask fails", in, "", docker.NoteNoDocker)
}

// The user's own DOCKER_HOST already beats a context and a builder, so nothing is asked.
// Mutation: ask regardless → Ask called, red.
func TestEngageUserHostSkipsQuestions(t *testing.T) {
	f, k := newFiles(t), &asker{}
	in := good(f, k)
	in.UserHost = "unix://" + f.ref
	k.a = docker.Answers{Endpoints: []string{"tcp://192.0.2.10:2376"}, BuilderDriver: "docker-container"}
	engage(t, "user host", in, f.ref, "")
	if k.calls != 0 {
		t.Errorf("Ask called %d times with the user's own DOCKER_HOST", k.calls)
	}
}

// A run without a Docker interest says nothing and asks nothing. Mutation: engage without
// interest → upstream, red.
func TestEngageNoInterestSilent(t *testing.T) {
	f, k := newFiles(t), &asker{}
	in := good(f, k)
	in.Interest = false
	engage(t, "no interest", in, "", "")
	if k.calls != 0 {
		t.Errorf("Ask called %d times without interest", k.calls)
	}
}

// Every note fits the hold's docker_note: at most 64 bytes of [a-z0-9:. -].
func TestNoteWords(t *testing.T) {
	notes := []string{docker.NoteNeedsEntry, docker.NoteSwitch, docker.NoteProbe, docker.NoteUpstream,
		docker.NoteRemoteContext, docker.NoteBuilder, docker.NoteNoDocker, docker.NoteAddress, docker.NoteStart,
		docker.NoteBlind, docker.NoteNoReading, docker.NoteOversized}
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
