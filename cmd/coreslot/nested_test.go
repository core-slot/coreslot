//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/wire"
)

// outer runs script under `coreslot run --mem mem -- sh -c script`, with $BIN the coreslot
// binary, $FDS the fds helper's command line and $D a scratch directory.
func outer(t *testing.T, dir string, mem string, script string, extraEnv ...string) result {
	t.Helper()
	args := []string{"run", "--restart=yes", "--mem", mem, "--", "sh", "-c", script}
	env := append(baseEnv(dir), "BIN="+cliBinary(t), "D="+t.TempDir(),
		"FDS=env "+helperEnv+"=fds "+cliBinary(t))
	return cli(t, append(env, extraEnv...), args...)
}

// fdsOf parses the fds helper's `fd N dev ino` lines.
func fdsOf(out string) map[int]string {
	fds := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 4 && f[0] == "fd" {
			n, _ := strconv.Atoi(f[1])
			fds[n] = f[2] + ":" + f[3]
		}
	}
	return fds
}

// inode is "dev:ino" of path.
func inode(t *testing.T, path string) string {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino))
}

func TestNestedPassThrough(t *testing.T) {
	dir := budgetDir(t, 1024)
	d := t.TempDir()
	r := outer(t, dir, "1G", `
		ln "$CORESLOT_STATE_DIR/holds/$CORESLOT_HOLD" "$D/held"
		cat "$CORESLOT_STATE_DIR/gen" > "$D/gen-before"
		ls "$CORESLOT_STATE_DIR/queue" > "$D/queue"
		"$BIN" run --mem 1536M --label inner -- $FDS || exit 1
		cat "$CORESLOT_STATE_DIR/gen" > "$D/gen-after"
		cmp -s "$D/gen-before" "$D/gen-after" || echo gen-changed
		ls "$CORESLOT_STATE_DIR/queue" | cmp -s - "$D/queue" || echo queue-changed`, "D="+d)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if strings.Contains(r.stdout, "changed") {
		t.Errorf("the nested run touched the queue or gen: %q", r.stdout)
	}
	env := envOf(r.stdout)
	if env["CORESLOT_GRANTED_MEM"] != "1024" || env["CORESLOT_HOLD"] == "" {
		t.Errorf("the nested command's environment: %v", env)
	}
	if got, want := fdsOf(r.stdout)[3], inode(t, filepath.Join(d, "held")); got != want {
		t.Errorf("fd 3 is %q, want the parent's hold (%s)", got, want)
	}
	if lines := runLog(t, dir); len(lines) != 1 || lines[0]["label"] == "inner" {
		t.Errorf("run-log lines %v: want the outer run's alone", lines)
	}
	budgetFree(t, dir)
}

// A run whose CORESLOT_HOLD names a live hold of an ancestor runs inside that reservation without
// queueing, though the budget is full; a dead hold or another process's live one makes it an
// ordinary request. Mutations: skipping the probe in DecideNested reddens "dead"; skipping
// isAncestor reddens "not an ancestor".
func TestNestedRunsInsideParent(t *testing.T) {
	t.Run("live ancestor", func(t *testing.T) {
		dir := budgetDir(t, 256)
		r := outer(t, dir, "256M", `"$BIN" run --no-wait --mem 256M -- true; echo "code=$?"`)
		if r.code != 0 || strings.TrimSpace(r.stdout) != "code=0" {
			t.Errorf("exit %d stdout %q stderr %q: want the nested run inside its parent, not queued", r.code, r.stdout, r.stderr)
		}
	})
	t.Run("not an ancestor", func(t *testing.T) {
		dir := budgetDir(t, 256)
		if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "true"); r.code != 0 {
			t.Fatalf("setup run: %d %q", r.code, r.stderr)
		}
		holdOther(t, dir, 256) // live, the whole budget, of a pid that is no ancestor
		other := entries(t, dir, "holds")[0]
		r := cli(t, append(baseEnv(dir), "CORESLOT_HOLD="+other), "run", "--restart=yes", "--no-wait", "--", "true")
		if r.code != 75 || !strings.Contains(r.stderr, "not admitted at once") {
			t.Errorf("exit %d stderr %q: want an ordinary request, refused by --no-wait", r.code, r.stderr)
		}
	})
	t.Run("dead", func(t *testing.T) {
		dir := stateDir(t, "")
		r := outer(t, dir, "256M", `echo "$CORESLOT_HOLD" > "$D/hold"`, "D="+t.TempDir())
		if r.code != 0 {
			t.Fatalf("setup run: %d %q", r.code, r.stderr)
		}
		dead := wire.HoldName(1, os.Getpid(), 1, "dead") // the test process is an ancestor
		if err := os.WriteFile(filepath.Join(dir, "holds", dead), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		r = cli(t, append(baseEnv(dir), "CORESLOT_HOLD="+dead), "run", "--restart=yes", "--no-wait", "--", "sh", "-c", `echo "$CORESLOT_HOLD"`)
		if got := strings.TrimSpace(r.stdout); r.code != 0 || got == dead || got == "" {
			t.Errorf("exit %d, the command's CORESLOT_HOLD %q: want a hold of its own, not the dead %s", r.code, got, dead)
		}
	})
}

func TestNestedSiblingsBothRun(t *testing.T) {
	dir := budgetDir(t, 1024)
	r := outer(t, dir, "1G", `
		"$BIN" run --mem 1G -- sh -c 'echo $CORESLOT_GRANTED_MEM > "$D/a"; sleep 1' &
		"$BIN" run --mem 1G -- sh -c 'echo $CORESLOT_GRANTED_MEM > "$D/b"; sleep 1' &
		wait
		cat "$D/a" "$D/b"`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.Fields(r.stdout); !slices.Equal(got, []string{"1024", "1024"}) {
		t.Errorf("siblings ran with %q, want 1024 and 1024 (nested runs are not budgeted against each other)", got)
	}
}

// The pass-through's clamp notice reads mem in bytes. Mutation: printing its amounts in MiB
// turns it red.
func TestNestedNoticeMem(t *testing.T) {
	dir := budgetDir(t, 8192)
	r := outer(t, dir, "1G", `"$BIN" run --mem 3G -- true`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if want := "coreslot: mem: requested 3 GiB, the parent run holds 1 GiB; running with 1 GiB"; !strings.Contains(r.stderr, want) {
		t.Errorf("stderr %q, want the notice %q", r.stderr, want)
	}
}

func TestNestedClampsAboveHeld(t *testing.T) {
	dir := budgetDir(t, 1024)
	r := outer(t, dir, "512M", `"$BIN" run --mem 1G -- sh -c 'echo $CORESLOT_GRANTED_MEM'`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.TrimSpace(r.stdout); got != "512" {
		t.Errorf("CORESLOT_GRANTED_MEM = %q, want 512", got)
	}
	if want := "coreslot: mem: requested 1 GiB, the parent run holds 512 MiB; running with 512 MiB"; !strings.Contains(r.stderr, want) {
		t.Errorf("stderr %q, want the notice %q", r.stderr, want)
	}
}

// A nested run inside a live parent hold, passing through or --independent, keeps its command in
// the parent command's process group, so a stop round's kill of that group leaves nothing of it.
// Mutation: giving the nested command a group of its own leaves its sleep running.
func TestNestedCommandDiesWithTheParentGroup(t *testing.T) {
	for _, flags := range []string{"", "--independent --mem 128M"} {
		t.Run("flags "+flags, func(t *testing.T) {
			dir := budgetDir(t, 1024)
			d := t.TempDir()
			outer(t, dir, "256M", `"$BIN" run --label inner `+flags+` -- sh -c 'echo $$ > "$D/pid"; exec sleep 600' >/dev/null 2>&1 &
				while [ ! -s "$D/pid" ]; do sleep 0.05; done; kill -KILL 0`, "D="+d)
			b, err := os.ReadFile(filepath.Join(d, "pid"))
			if err != nil {
				t.Fatal(err)
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			t.Cleanup(func() { _ = unix.Kill(pid, unix.SIGKILL) })
			for deadline := time.Now().Add(5 * time.Second); !exited(pid); time.Sleep(50 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("the nested command (pid %d) outlived the kill of its parent's group", pid)
				}
			}
		})
	}
}

// exited reports whether pid has exited; a zombie that nobody reaps (a container's init may not)
// has exited too.
func exited(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	return unix.Kill(pid, 0) != nil || err == nil && strings.Contains(string(b), ") Z ")
}

func TestIndependentQueues(t *testing.T) {
	dir := budgetDir(t, 256)
	r := outer(t, dir, "256M", `"$BIN" run --independent --timeout 1s -- true; echo "code=$?"`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if strings.TrimSpace(r.stdout) != "code=75" || !strings.Contains(r.stderr, "not admitted within 1s") {
		t.Errorf("stdout %q stderr %q: an --independent run must queue behind its parent", r.stdout, r.stderr)
	}
}

func TestIndependentDropsInheritedContext(t *testing.T) {
	dir := budgetDir(t, 512)
	d := t.TempDir()
	r := outer(t, dir, "256M", `echo "$CORESLOT_HOLD" > "$D/hold"; ln "$CORESLOT_STATE_DIR/holds/$CORESLOT_HOLD" "$D/held"
		exec 7</dev/null; "$BIN" run --independent --mem 128M -- $FDS`, "D="+d)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	parentHold, err := os.ReadFile(filepath.Join(d, "hold"))
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(r.stdout)
	if env["CORESLOT_GRANTED_MEM"] != "128" || env["CORESLOT_HOLD"] == "" ||
		env["CORESLOT_HOLD"] == strings.TrimSpace(string(parentHold)) {
		t.Errorf("the command's environment %v: want only its own grant", env)
	}
	// The helper's own runtime may open descriptors of its own, so check identities, not numbers.
	fds := fdsOf(r.stdout)
	parent := inode(t, filepath.Join(d, "held"))
	for fd, id := range fds {
		if id == parent {
			t.Errorf("fd %d is the parent's hold, which was passed on: %v", fd, fds)
		}
	}
	if fds[7] != inode(t, "/dev/null") {
		t.Errorf("fd 7, an unrelated inherited descriptor, was not passed through: %v", fds)
	}
}

func TestNestedUnderUnbudgetedParent(t *testing.T) {
	show := `echo "$CORESLOT_GRANTED_MEM $CORESLOT_UNBUDGETED"`

	t.Run("fresh directory", func(t *testing.T) {
		dir := stateDir(t, "")
		env := append(baseEnv(dir), "CORESLOT_UNBUDGETED=parent-reason", "CORESLOT_GRANTED_MEM=640")
		r := cli(t, env, "run", "--restart=yes", "--mem", "384M", "--", "sh", "-c", show)
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		if got := strings.TrimSpace(r.stdout); got != "384 parent-reason" {
			t.Errorf("the command saw %q, want its 384 MiB and the parent's reason", got)
		}
		if !strings.Contains(r.stderr, "unbudgeted") {
			t.Errorf("stderr %q: want a notice", r.stderr)
		}
		if names := entries(t, dir, ""); len(names) != 0 {
			t.Errorf("an unbudgeted pass-through wrote %q", names)
		}
		if strings.Contains(r.stderr, "the run log was not written") {
			t.Errorf("stderr %q: a missing FORMAT is reported, want silence", r.stderr)
		}
	})

	// The inherited reason is untrusted text: the notice escapes it like a label, and the command
	// still receives it unchanged. Mutation: print the reason raw in passThroughUnbudgeted.
	t.Run("the notice escapes the reason", func(t *testing.T) {
		dir := stateDir(t, "")
		reason := "evil\x1b[2J\nsecond line"
		env := append(baseEnv(dir), "CORESLOT_UNBUDGETED="+reason)
		r := cli(t, env, "run", "--restart=yes", "--", "sh", "-c", `printf %s "$CORESLOT_UNBUDGETED"`)
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		want := `the parent run is unbudgeted (evil\x1b[2J\nsecond line); running unbudgeted with `
		if raw := rawTerminalControls([]byte(r.stderr)); len(raw) != 0 || !strings.Contains(r.stderr, want) {
			t.Errorf("stderr %q: want the reason escaped on the notice's line, %q", r.stderr, want)
		}
		if r.stdout != reason {
			t.Errorf("the command saw CORESLOT_UNBUDGETED %q, want %q unchanged", r.stdout, reason)
		}
	})

	t.Run("format understood", func(t *testing.T) {
		dir := stateDir(t, "")
		if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "true"); r.code != 0 {
			t.Fatalf("setup run: %d %q", r.code, r.stderr)
		}
		gen, _ := os.ReadFile(filepath.Join(dir, "gen"))
		env := append(baseEnv(dir), "CORESLOT_UNBUDGETED=parent-reason")
		if r := cli(t, env, "run", "--restart=yes", "--mem", "384M", "--label", "nested-unbudgeted", "--", "true"); r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		if after, _ := os.ReadFile(filepath.Join(dir, "gen")); string(after) != string(gen) {
			t.Errorf("gen changed: %q -> %q", gen, after)
		}
		if _, err := os.Stat(filepath.Join(dir, "degraded")); err == nil {
			t.Error("the pass-through wrote a degraded record")
		}
		if lines := runLog(t, dir); len(lines) != 1 {
			t.Errorf("run-log lines %v: want the setup run's alone", lines)
		}
	})

}
