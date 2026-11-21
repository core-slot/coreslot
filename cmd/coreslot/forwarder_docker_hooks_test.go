//go:build coreslot_testhooks && darwin

package main

import (
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
	relay "github.com/core-slot/coreslot/internal/forward"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/testhooks"
)

// forwardSeamEnv makes the forwarder of a test binary acting as coreslot what its value names:
// silent never says ready.
const forwardSeamEnv = "CORESLOT_CLI_TEST_FORWARD"

func init() {
	if os.Getenv(forwardSeamEnv) == "silent" {
		serveForward = func(cfg relay.Config) error {
			_, err := io.Copy(io.Discard, cfg.End)
			return err
		}
	}
}

// fwBed is a run against the HTTP fake engine as the hook socket, in a state directory short
// enough for the forwarder's address, with a work directory its commands write into.
type fwBed struct {
	dir, work, hooks string
	fake             *dockertest.Fake
	env              []string
}

// newFwBed makes a bed whose needs file holds needs; DOCKER_HOST is unset in its environment.
func newFwBed(t *testing.T, needs string, opts ...dockertest.Option) *fwBed {
	t.Helper()
	b := &fwBed{dir: filepath.Join(dockertest.SocketDir(t), "s"), work: t.TempDir(), hooks: t.TempDir()}
	if err := os.Mkdir(b.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"config.toml": "[memory]\nreserve = \"0\"\n[watch]\ncalm_s = 0\n", "needs.toml": needs} {
		if err := os.WriteFile(filepath.Join(b.dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	budgetRoom.Store(b.dir, int64(1024))
	b.fake = dockertest.HTTP(t, opts...)
	for _, kv := range baseEnv(b.dir) { // no Docker host, context or configuration of the machine's
		if !strings.HasPrefix(kv, "DOCKER_HOST=") && !strings.HasPrefix(kv, "DOCKER_CONTEXT=") && !strings.HasPrefix(kv, "DOCKER_CONFIG=") {
			b.env = append(b.env, kv)
		}
	}
	b.env = append(b.env, "DOCKER_CONFIG="+t.TempDir(), "CORESLOT_TESTHOOK_DIR="+b.hooks, "CORESLOT_TESTHOOK_DOCKER_SOCKET="+b.fake.Sock,
		"CORESLOT_TESTHOOK_HEADROOM_MIB=1024", "FWT="+b.work)
	b.readings(calm(0))
	return b
}

// calm is the readings of a machine with ample room; floor those of one whose swap room is gone.
func calm(swapUsedMiB int64) map[string]int64 {
	return map[string]int64{"Decomp": 0, "SwapIns": 0, "HasPSI": 0, "SwapUsedMiB": swapUsedMiB,
		"SwapFreeMiB": 1 << 20, "VMVolumeFreeMiB": 1 << 20}
}

func floor(swapUsedMiB int64) map[string]int64 {
	r := calm(swapUsedMiB)
	r["SwapFreeMiB"], r["VMVolumeFreeMiB"], r["MemAvailableMiB"] = 0, 0, 0
	return r
}

// readings replaces the machine readings every run of the bed reads.
func (b *fwBed) readings(nums map[string]int64) {
	var s strings.Builder
	for name, n := range nums {
		s.WriteString(name + " " + strconv.FormatInt(n, 10) + "\n")
	}
	path := filepath.Join(b.hooks, testhooks.ReadingsFile)
	if err := os.WriteFile(path+".partial", []byte(s.String()), 0o600); err != nil {
		panic(err)
	}
	if err := os.Rename(path+".partial", path); err != nil {
		panic(err)
	}
}

// file is the work directory's file name, "" when it does not exist.
func (b *fwBed) file(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(b.work, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(body)
}

// hold is the hold body the command saved as $FWT/hold.
func (b *fwBed) hold(t *testing.T) map[string]any {
	t.Helper()
	var h map[string]any
	if err := json.Unmarshal([]byte(b.file(t, "hold")), &h); err != nil {
		t.Fatalf("the command saved no hold: %v", err)
	}
	return h
}

// Shell fragments of a command: save its DOCKER_HOST, call the engine through it, and save its hold
// after the first tick.
const (
	saveHost = `echo "$DOCKER_HOST" > "$FWT/host"; `
	callInfo = `curl -s --unix-socket "${DOCKER_HOST#unix://}" http://docker/info > "$FWT/info"; `
	saveHold = `sleep 2.5; cat "$CORESLOT_STATE_DIR/holds/$CORESLOT_HOLD" > "$FWT/hold"; `
)

// run runs a labelled shell command to its end.
func (b *fwBed) run(t *testing.T, label, script string, env ...string) result {
	t.Helper()
	return cli(t, append(append([]string{}, b.env...), env...), "run", "--restart=yes", "--label", label, "--mem", "64M", "--", "sh", "-c", script)
}

// forced is a needs file that gives label a forwarder whatever the command.
func forced(label string) string { return "[needs.\"" + label + "\"]\ndocker_forward = true\n" }

// forwarderPID is the pid of the forwarder serving entry, 0 when none runs.
func forwarderPID(t *testing.T, entry string) int {
	t.Helper()
	out, _ := exec.Command("pgrep", "-f", "forward "+entry+" ").Output()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return pid
}

// entryOf is the entry the address in host belongs to.
func entryOf(t *testing.T, host string) string {
	t.Helper()
	addr, ok := strings.CutPrefix(strings.TrimSpace(host), "unix://")
	if !ok || filepath.Base(addr) != relay.SockName {
		t.Fatalf("DOCKER_HOST %q names no forwarder's address", host)
	}
	return filepath.Dir(addr)
}

func count(s, sub string) int { return strings.Count(s, sub) }

// An engaged run's command finds DOCKER_HOST naming its forwarder's address in the forwarder's
// directory of the state directory, and a call through it reaches the engine. Mutation: leaving
// withDockerHost out of admitted gives the command no DOCKER_HOST.
func TestEngagedRunSetsDockerHost(t *testing.T) {
	b := newFwBed(t, forced("fw"))
	if r := b.run(t, "fw", saveHost+callInfo); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	entry := entryOf(t, b.file(t, "host"))
	real, err := filepath.EvalSymlinks(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(entry) != filepath.Join(real, relay.DirName) {
		t.Errorf("entry %s is not in %s", entry, filepath.Join(real, relay.DirName))
	}
	if !strings.Contains(b.file(t, "info"), "Docker Desktop") || !strings.Contains(strings.Join(b.fake.Requests(), "\n"), "GET /info") {
		t.Errorf("the call through the address did not reach the engine: %q, requests %q", b.file(t, "info"), b.fake.Requests())
	}
}

// A run whose label has no Docker interest is not engaged and says nothing: its command sees the
// user's DOCKER_HOST and its hold carries no note. Mutation: Interest always true in engage gives
// the command a forwarder's address.
func TestNotEngagedEnvUntouched(t *testing.T) {
	b := newFwBed(t, "")
	user := "unix://" + b.fake.Sock
	r := b.run(t, "plain", saveHost+saveHold, "DOCKER_HOST="+user)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := strings.TrimSpace(b.file(t, "host")); got != user {
		t.Errorf("DOCKER_HOST %q, want the user's %q", got, user)
	}
	if strings.Contains(r.stderr, "no-forwarder") {
		t.Errorf("a run without interest said %q", r.stderr)
	}
	if note, ok := b.hold(t)["docker_note"]; ok {
		t.Errorf("hold carries docker_note %v", note)
	}
}

// A forwarder that does not start leaves the command's environment as it was, with one notice and
// the note in the hold: one that never says ready, and one whose entry cannot be made (a forwarder's
// directory without write permission, as a sandbox gives). Mutation: startForwarder taking a
// forwarder that did not say ready gives the command DOCKER_HOST and the hold no note.
func TestForwarderStartFailureRunsUntouched(t *testing.T) {
	for _, c := range []struct {
		name     string
		env      []string
		readOnly bool
	}{
		{"never ready", []string{forwardSeamEnv + "=silent"}, false},
		{"entry cannot be made", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newFwBed(t, forced("fw"))
			if c.readOnly {
				real, err := filepath.EvalSymlinks(b.dir)
				if err != nil {
					t.Fatal(err)
				}
				fw := filepath.Join(real, relay.DirName)
				if err := os.Mkdir(fw, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(fw, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(fw, 0o700) })
			}
			r := b.run(t, "fw", saveHost+saveHold, c.env...)
			if r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			if got := strings.TrimSpace(b.file(t, "host")); got != "" {
				t.Errorf("DOCKER_HOST %q, want it unset as the user had it", got)
			}
			if n := count(r.stderr, docker.NoteStart); n != 1 {
				t.Errorf("%d notices naming %q: %q", n, docker.NoteStart, r.stderr)
			}
			if note := b.hold(t)["docker_note"]; note != docker.NoteStart {
				t.Errorf("hold's docker_note %v, want %q", note, docker.NoteStart)
			}
		})
	}
}

// A command that leaves an events stream running in the background and exits 3: the run returns 3
// at once, the address is a symlink to the engine from then on, and the forwarder serves the stream
// until it closes, then exits. Mutations: endForwarder killing the forwarder's group leaves no
// forwarder to serve the stream; endForwarder waiting for it makes the run return only when the
// stream ends.
func TestForwarderOutlivesCommandWhileStreamOpen(t *testing.T) {
	b := newFwBed(t, forced("fw"))
	start := time.Now()
	r := b.run(t, "fw", saveHost+`curl -sN --unix-socket "${DOCKER_HOST#unix://}" http://docker/events > "$FWT/events" 2>/dev/null </dev/null & echo $! > "$FWT/curl"; sleep 1.5; exit 3`)
	took := time.Since(start)
	if r.code != 3 || took > 10*time.Second {
		t.Fatalf("exit %d after %v, want 3 at once: %s", r.code, took, r.stderr)
	}
	entry := entryOf(t, b.file(t, "host"))
	if fi, err := os.Lstat(filepath.Join(entry, relay.SockName)); err != nil || fi.Mode().Type() != fs.ModeSymlink {
		t.Errorf("the address after the run is not a symlink: %v", err)
	}
	pid := forwarderPID(t, entry)
	if pid == 0 {
		t.Fatal("no forwarder serves the open stream after the run")
	}
	lines := count(b.file(t, "events"), "\n")
	waitFor(t, 5*time.Second, "the stream to go on", func() bool { return count(b.file(t, "events"), "\n") > lines })
	curl, err := strconv.Atoi(strings.TrimSpace(b.file(t, "curl")))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(curl, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the forwarder to exit", func() bool { return unix.Kill(pid, 0) != nil })
}

// A forwarder killed under a running command: the run points the address at the engine within a
// tick and is blind, and a call through the address still reaches the engine, also when the
// entry was removed first (a walk), which the repair recreates. Mutation: no repair once the
// forwarder exited leaves the address dead.
func TestForwarderKilledRunRepairs(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run("removed "+strconv.FormatBool(removed), func(t *testing.T) {
			b := newFwBed(t, forced("fw"))
			done := make(chan result, 1)
			go func() {
				done <- b.run(t, "fw", saveHost+`while [ ! -e "$FWT/go" ]; do sleep 0.1; done; `+saveHold+callInfo)
			}()
			waitFor(t, 30*time.Second, "the command", func() bool { return b.file(t, "host") != "" })
			entry := entryOf(t, b.file(t, "host"))
			pid := forwarderPID(t, entry)
			if pid == 0 {
				t.Fatal("no forwarder")
			}
			if removed {
				if err := os.RemoveAll(entry); err != nil {
					t.Fatal(err)
				}
			}
			if err := unix.Kill(pid, unix.SIGKILL); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 2*time.Second, "the address to be repaired", func() bool {
				fi, err := os.Lstat(filepath.Join(entry, relay.SockName))
				return err == nil && fi.Mode().Type() == fs.ModeSymlink
			})
			if err := os.WriteFile(filepath.Join(b.work, "go"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if r := <-done; r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			if note := b.hold(t)["docker_note"]; note != docker.NoteBlind {
				t.Errorf("hold's docker_note %v, want %q", note, docker.NoteBlind)
			}
			if !strings.Contains(b.file(t, "info"), "Docker Desktop") {
				t.Errorf("the call after the repair did not reach the engine: %q", b.file(t, "info"))
			}
		})
	}
}

// The command of an engaged run inherits no end of the forwarder's pipes: its descriptors from 3
// are its hold's alone. Mutation: clearing close-on-exec on the run's end of a pipe adds it.
func TestPipesNotInherited(t *testing.T) {
	b := newFwBed(t, forced("fw"))
	r := cli(t, b.env, append([]string{"run", "--restart=yes", "--label", "fw", "--mem", "64M", "--"}, helper(t, "fds")...)...)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var fds []string
	for _, line := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(line, "fd ") {
			fds = append(fds, strings.Fields(line)[1])
		}
	}
	if strings.Join(fds, " ") != "3" {
		t.Errorf("the command's descriptors from 3 are %q, want the hold's 3 alone:\n%s", fds, r.stdout)
	}
	if !strings.Contains(r.stdout, "env CORESLOT_HOLD=") {
		t.Errorf("the command ran outside the budget:\n%s", r.stdout)
	}
}

// A command a memory stop killed comes back to the forwarder it had: the restart starts none.
// (The restart's sequence is the conformance suite's: an older running job, a learned share, the
// floor until the kill, then calm.)
// Mutation: a forwarder started at every admission gives the second attempt another pid.
func TestForwarderAcrossRestart(t *testing.T) {
	b := newFwBed(t, forced("fw"))
	e, err := openEngine(location{dir: b.dir}, false)
	if err != nil {
		t.Fatal(err)
	}
	share := 0.1 // a learned share, so that a stop round lists the run
	if err := e.Store().AppendRun(store.RunRecord{TimeNS: store.NowNS(), Label: "fw", TickShare: &share}); err != nil {
		t.Fatal(err)
	}
	older := cliCmd(t, b.env, "run", "--restart=yes", "--label", "older", "--mem", "64M", "--", "sh", "-c", `echo up > "$FWT/older"; sleep 120`)
	if err := older.Start(); err != nil { // a stop round never takes the oldest running job
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the older run", func() bool { return b.file(t, "older") != "" })
	time.Sleep(3 * time.Second) // past its first tick
	script := `echo "$DOCKER_HOST" >> "$FWT/hosts"; a=${DOCKER_HOST#unix://}; pgrep -f "forward ${a%/docker.sock} " >> "$FWT/pids"; ` +
		`[ "$(wc -l < "$FWT/hosts")" -ge 2 ] && exit 0; echo $$ > "$FWT/first"; sleep 60`
	done := make(chan result, 1)
	go func() { done <- b.run(t, "fw", script) }()
	waitFor(t, 30*time.Second, "the first attempt", func() bool { return b.file(t, "first") != "" })
	first, err := strconv.Atoi(strings.TrimSpace(b.file(t, "first")))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second) // past the attempt's first tick
	b.readings(floor(1024))
	waitFor(t, 30*time.Second, "the stop", func() bool { return unix.Kill(first, 0) != nil })
	b.readings(calm(2048))
	r := <-done
	if r.code != 0 || !strings.Contains(r.stderr, "memory stop 1 of 3") {
		t.Fatalf("exit %d, want a stop and a restart: %s", r.code, r.stderr)
	}
	_ = unix.Kill(-older.Process.Pid, unix.SIGKILL)
	hosts, pids := strings.Fields(b.file(t, "hosts")), strings.Fields(b.file(t, "pids"))
	if len(hosts) != 2 || hosts[0] != hosts[1] {
		t.Errorf("the attempts' DOCKER_HOST %q, want one address twice", hosts)
	}
	if len(pids) != 2 || pids[0] != pids[1] {
		t.Errorf("forwarder pids %q, want the same for both attempts", pids)
	}
}

// A command that is no container client by its name, and connects through its forwarder, is
// container work from the next tick: never a stop's victim. Mutation: the connected event left
// unread keeps the hold a victim.
func TestConnectionMarksContainerWork(t *testing.T) {
	b := newFwBed(t, forced("fw"))
	if r := b.run(t, "fw", callInfo+saveHold); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if h := b.hold(t); h["container"] != true || h["victim"] != false {
		t.Errorf("hold after a connection: container %v, victim %v; want true, false", h["container"], h["victim"])
	}
}

// A command that bypasses its forwarder, holding the engine's socket itself, is container work by
// the client scan, as a run without a forwarder is: never a stop's victim, and its containers'
// memory unmeasured (no Docker footprint). Mutation: the scan skipped under a forwarder keeps the
// hold a victim.
func TestBypassingClientIsContainerWork(t *testing.T) {
	b := newFwBed(t, forced("fw"))
	hold := `curl -sN --unix-socket "$ENGINE" http://docker/events > /dev/null & ` + saveHold + `kill $!; `
	if r := b.run(t, "fw", hold, "ENGINE="+b.fake.Sock); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	h := b.hold(t)
	if _, fp := h["docker_footprint_mib"]; h["container"] != true || h["victim"] != false || fp {
		t.Errorf("hold of a bypassing client: container %v, victim %v, docker_footprint_mib %v; want true, false, absent",
			h["container"], h["victim"], h["docker_footprint_mib"])
	}
}

// A docker_forward that is not a boolean gives a container command no forwarder, its note in the
// hold and one notice of it; docker_forward = false gives the note in the hold and no notice, the
// user having chosen it. Mutations: EntryBad left out of engage engages the run; setEngagement
// saying every note prints the switch's.
func TestBadDockerForwardNote(t *testing.T) {
	b := newFwBed(t, "[needs.\"bad\"]\ndocker_forward = \"x\"\n[needs.\"off\"]\ndocker_forward = false\n")
	command := filepath.Join(b.work, "docker")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"+saveHost+saveHold+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for label, want := range map[string]string{"bad": docker.NoteNeedsEntry, "off": docker.NoteSwitch} {
		r := cli(t, b.env, "run", "--restart=yes", "--label", label, "--mem", "64M", "--", command)
		if r.code != 0 {
			t.Fatalf("%s: exit %d: %s", label, r.code, r.stderr)
		}
		wantNotices := map[string]int{"bad": 1, "off": 0}[label]
		if count(r.stderr, "no-forwarder") != wantNotices || count(r.stderr, "needs file") != wantNotices {
			t.Errorf("%s: want %d notes and needs-file notices:\n%s", label, wantNotices, r.stderr)
		}
		if note := b.hold(t)["docker_note"]; note != want || strings.TrimSpace(b.file(t, "host")) != "" {
			t.Errorf("%s: hold's docker_note %v, DOCKER_HOST %q; want %q and none", label, note, b.file(t, "host"), want)
		}
	}
}
