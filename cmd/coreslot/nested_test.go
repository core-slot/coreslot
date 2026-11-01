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

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/wire"
)

// outer runs script under `coreslot run --mem mem -- sh -c script`, with $BIN the coreslot
// binary, $FDS the fds helper's command line and $D a scratch directory.
func outer(t *testing.T, dir string, mem string, script string, extraEnv ...string) result {
	t.Helper()
	args := []string{"run", "--mem", mem, "--", "sh", "-c", script}
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

func nestedLines(t *testing.T, dir string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range runLog(t, dir) {
		if l["nested"] == true {
			out = append(out, l)
		}
	}
	return out
}

func TestNestedPassThrough(t *testing.T) {
	dir := stateDir(t, "[resources]\nmem = \"1G\"\n")
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
	if env["CORESLOT_GRANTED_MEM"] != "1024" || env["CORESLOT_HELD_FD"] != "3" || env["CORESLOT_HOLD"] == "" {
		t.Errorf("the nested command's environment: %v", env)
	}
	if got, want := fdsOf(r.stdout)[3], inode(t, filepath.Join(d, "held")); got != want {
		t.Errorf("fd 3 is %q, want the parent's hold (%s)", got, want)
	}
	lines := nestedLines(t, dir)
	if len(lines) != 1 || lines[0]["label"] != "inner" {
		t.Errorf("nested run-log lines %v: want one for inner", lines)
	}
	budgetFree(t, dir)
}

// A run whose CORESLOT_HOLD names a live hold of an ancestor runs inside that reservation without
// queueing, though the budget is full; a dead hold or another process's live one makes it an
// ordinary request. Mutations: skipping the probe in DecideNested reddens "dead"; skipping
// isAncestor reddens "not an ancestor".
func TestNestedRunsInsideParent(t *testing.T) {
	t.Run("live ancestor", func(t *testing.T) {
		dir := stateDir(t, "[resources]\nmem = \"256M\"\n")
		r := outer(t, dir, "256M", `env -u CORESLOT_TOKEN_FDS "$BIN" run --no-wait --mem 256M -- true; echo "code=$?"`)
		if r.code != 0 || strings.TrimSpace(r.stdout) != "code=0" {
			t.Errorf("exit %d stdout %q stderr %q: want the nested run inside its parent, not queued", r.code, r.stdout, r.stderr)
		}
	})
	t.Run("not an ancestor", func(t *testing.T) {
		dir := stateDir(t, "[resources]\nmem = \"256M\"\n")
		if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
			t.Fatalf("setup run: %d %q", r.code, r.stderr)
		}
		holdOther(t, dir, 2) // live, the whole budget, of a pid that is no ancestor
		other := entries(t, dir, "holds")[0]
		r := cli(t, append(baseEnv(dir), "CORESLOT_HOLD="+other), "run", "--no-wait", "--", "true")
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
		r = cli(t, append(baseEnv(dir), "CORESLOT_HOLD="+dead), "run", "--no-wait", "--", "sh", "-c", `echo "$CORESLOT_HOLD"`)
		if got := strings.TrimSpace(r.stdout); r.code != 0 || got == dead || got == "" {
			t.Errorf("exit %d, the command's CORESLOT_HOLD %q: want a hold of its own, not the dead %s", r.code, got, dead)
		}
		if lines := nestedLines(t, dir); len(lines) != 0 {
			t.Errorf("nested run-log lines %v: an ordinary request is not nested", lines)
		}
	})
}

func TestNestedSiblingsBothRun(t *testing.T) {
	dir := stateDir(t, "[resources]\nmem = \"1G\"\n")
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
	dir := stateDir(t, "[resources]\nmem = \"8G\"\n")
	r := outer(t, dir, "1G", `"$BIN" run --mem 3G -- true`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if want := "coreslot: mem: requested 3 GiB, the parent run holds 1 GiB; running with 1 GiB"; !strings.Contains(r.stderr, want) {
		t.Errorf("stderr %q, want the notice %q", r.stderr, want)
	}
}

func TestNestedClampsAboveHeld(t *testing.T) {
	dir := stateDir(t, "[resources]\nmem = \"1G\"\n")
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

func TestIndependentQueues(t *testing.T) {
	dir := stateDir(t, "[resources]\nmem = \"256M\"\n")
	r := outer(t, dir, "256M", `"$BIN" run --independent --timeout 1s -- true; echo "code=$?"`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if strings.TrimSpace(r.stdout) != "code=75" || !strings.Contains(r.stderr, "not admitted within 1s") {
		t.Errorf("stdout %q stderr %q: an --independent run must queue behind its parent", r.stdout, r.stderr)
	}
}

func TestIndependentDropsInheritedContext(t *testing.T) {
	dir := stateDir(t, "[resources]\nmem = \"512M\"\n")
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
	if env["CORESLOT_GRANTED_MEM"] != "128" || env["CORESLOT_HELD_FD"] != "3" || env["CORESLOT_HOLD"] == "" ||
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
		r := cli(t, env, "run", "--mem", "384M", "--", "sh", "-c", show)
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
		r := cli(t, env, "run", "--", "sh", "-c", `printf %s "$CORESLOT_UNBUDGETED"`)
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
		if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
			t.Fatalf("setup run: %d %q", r.code, r.stderr)
		}
		gen, _ := os.ReadFile(filepath.Join(dir, "gen"))
		env := append(baseEnv(dir), "CORESLOT_UNBUDGETED=parent-reason")
		if r := cli(t, env, "run", "--mem", "384M", "--label", "nested-unbudgeted", "--", "true"); r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		if after, _ := os.ReadFile(filepath.Join(dir, "gen")); string(after) != string(gen) {
			t.Errorf("gen changed: %q -> %q", gen, after)
		}
		if _, err := os.Stat(filepath.Join(dir, "degraded")); err == nil {
			t.Error("the pass-through wrote a degraded record")
		}
		lines := nestedLines(t, dir)
		if len(lines) != 1 || lines[0]["label"] != "nested-unbudgeted" {
			t.Errorf("nested run-log lines %v", lines)
		}
	})

	// A pass-through re-reads FORMAT immediately before its line too (PROTOCOL.md section 20):
	// under a FORMAT it does not understand it writes none, and says so. Only a FORMAT that does
	// not exist is silent.
	for name, replace := range map[string]func(t *testing.T, path string){
		"format 3": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("coreslot-protocol 3\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"FORMAT a directory": func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"FORMAT a symlink": func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "FORMAT")
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t, "")
			if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
				t.Fatalf("setup run: %d %q", r.code, r.stderr)
			}
			replace(t, filepath.Join(dir, "FORMAT"))
			env := append(baseEnv(dir), "CORESLOT_UNBUDGETED=parent-reason")
			r := cli(t, env, "run", "--mem", "384M", "--label", "nested-format", "--", "true")
			if r.code != 0 {
				t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
			}
			if lines := nestedLines(t, dir); len(lines) != 0 {
				t.Errorf("a pass-through wrote a run-log line: %v", lines)
			}
			if !strings.Contains(r.stderr, "the run log was not written") {
				t.Errorf("stderr %q: want a notice that the run log was not written", r.stderr)
			}
		})
	}
}
