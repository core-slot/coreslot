//go:build unix

package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// outer runs script under `coreslot run need -- sh -c script`, with $BIN the coreslot binary,
// $FDS the fds helper's command line and $D a scratch directory.
func outer(t *testing.T, dir string, need []string, script string, extraEnv ...string) result {
	t.Helper()
	args := []string{"run"}
	for _, n := range need {
		args = append(args, "--need", n)
	}
	args = append(args, "--", "sh", "-c", script)
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

func TestNestedGrant(t *testing.T) {
	req := spec.Request{Need: map[string]spec.Range{
		"cpu":      {Min: 4, Max: 12},
		"gpu":      {Min: 3, Max: 3},
		"pg-local": {Min: 1, Max: 1},
	}}
	grant, clamped, unbudgeted := nestedGrant(req, map[string]int{"cpu": 8, "gpu": 2})
	if want := map[string]int{"cpu": 8, "gpu": 2, "pg-local": 1}; !maps.Equal(grant, want) {
		t.Errorf("grant %v, want %v", grant, want)
	}
	if want := map[string]int{"gpu": 3}; !maps.Equal(clamped, want) {
		t.Errorf("clamped %v, want %v", clamped, want)
	}
	if !slices.Equal(unbudgeted, []string{"pg-local"}) {
		t.Errorf("unbudgeted %v, want [pg-local]", unbudgeted)
	}
	grant, _, _ = nestedGrant(spec.Request{Need: map[string]spec.Range{"cpu": {Min: 2, Max: 4}}}, map[string]int{"cpu": 10})
	if grant["cpu"] != 4 {
		t.Errorf("min(max, held) = %d, want 4", grant["cpu"])
	}
}

func TestNestedPassThrough(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 8\n")
	r := outer(t, dir, []string{"cpu=8"}, `
		cat "$CORESLOT_STATE_DIR/gen" > "$D/gen-before"
		ls "$CORESLOT_STATE_DIR/queue" > "$D/queue"
		"$BIN" run --need cpu=4..12 --label inner -- $FDS || exit 1
		cat "$CORESLOT_STATE_DIR/gen" > "$D/gen-after"
		cmp -s "$D/gen-before" "$D/gen-after" || echo gen-changed
		ls "$CORESLOT_STATE_DIR/queue" | cmp -s - "$D/queue" || echo queue-changed`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if strings.Contains(r.stdout, "changed") {
		t.Errorf("the nested run touched the queue or gen: %q", r.stdout)
	}
	env := envOf(r.stdout)
	if env["CORESLOT_GRANTED_CPU"] != "8" || env["CORESLOT_TOKEN_FDS"] != "4-11" || env["CORESLOT_HELD_FD"] != "3" {
		t.Errorf("the nested command's environment: %v", env)
	}
	fds := fdsOf(r.stdout)
	for i := 0; i < 8; i++ {
		if got, want := fds[4+i], inode(t, filepath.Join(dir, "res", "cpu", strconv.Itoa(i))); got != want {
			t.Errorf("fd %d is %q, want token cpu/%d (%s)", 4+i, got, i, want)
		}
	}
	lines := nestedLines(t, dir)
	if len(lines) != 1 || lines[0]["label"] != "inner" || fmt.Sprint(lines[0]["grant"]) != "map[cpu:8]" {
		t.Errorf("nested run-log lines %v: want one for inner with cpu 8", lines)
	}
	tokensFree(t, dir, 8)
}

func TestNestedSiblingsBothRun(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 8\n")
	r := outer(t, dir, []string{"cpu=8"}, `
		"$BIN" run --need cpu=1..8 -- sh -c 'echo $CORESLOT_GRANTED_CPU > "$D/a"; sleep 1' &
		"$BIN" run --need cpu=1..8 -- sh -c 'echo $CORESLOT_GRANTED_CPU > "$D/b"; sleep 1' &
		wait
		cat "$D/a" "$D/b"`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.Fields(r.stdout); !slices.Equal(got, []string{"8", "8"}) {
		t.Errorf("siblings ran with %q, want 8 and 8 (nested runs are not budgeted against each other)", got)
	}
}

func TestNestedClampsAboveHeld(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 8\n")
	r := outer(t, dir, []string{"cpu=4"}, `"$BIN" run --need cpu=8..8 -- echo {n}`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.TrimSpace(r.stdout); got != "4" {
		t.Errorf("{n} = %q, want 4", got)
	}
	if want := "coreslot: cpu: requested 8, the parent run holds 4; running with 4"; !strings.Contains(r.stderr, want) {
		t.Errorf("stderr %q, want the notice %q", r.stderr, want)
	}
}

func TestNestedUndeclaredInParent(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 8\npg-local = 2\n")
	r := outer(t, dir, []string{"cpu=2"}, `"$BIN" run --need pg-local=1 -- sh -c 'echo $CORESLOT_GRANTED_PG_LOCAL $CORESLOT_GRANTED_CPU'`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.TrimSpace(r.stdout); got != "1 2" {
		t.Errorf("the command saw %q, want pg-local 1 and the parent's cpu 2", got)
	}
	if !strings.Contains(r.stderr, "pg-local") || !strings.Contains(r.stderr, "unbudgeted") {
		t.Errorf("stderr %q, want a notice for pg-local", r.stderr)
	}
	if locked(t, dir, "pg-local", 0) || locked(t, dir, "pg-local", 1) {
		t.Error("the nested run locked a pg-local token")
	}
}

func TestNestedCountsFromDescriptorsNotEnv(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 8\n")
	r := outer(t, dir, []string{"cpu=8"}, `CORESLOT_GRANTED_CPU=99 "$BIN" run --need cpu=1..64 -- echo {n}`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.TrimSpace(r.stdout); got != "8" {
		t.Errorf("{n} = %q, want 8: held is counted from the descriptors", got)
	}
}

func TestDescriptorClosingLauncherRefuses(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	r := outer(t, dir, []string{"cpu=4"}, `
		start=$(date +%s)
		sh -c 'exec 4>&-; exec "$BIN" run -- true'
		code=$?
		echo "code=$code took=$(( $(date +%s) - start ))"`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	out := strings.TrimSpace(r.stdout)
	if !strings.HasPrefix(out, "code=75 ") {
		t.Fatalf("the nested run reported %q, want a refusal (stderr %q)", out, r.stderr)
	}
	if took, _ := strconv.Atoi(strings.TrimPrefix(out, "code=75 took=")); took > 2 {
		t.Errorf("refused after %d s, want within 2 s (load: %s)", took, uptime())
	}
	if !strings.Contains(r.stderr, "descriptor 4 of CORESLOT_TOKEN_FDS") || !strings.Contains(r.stderr, "closed") ||
		!strings.Contains(r.stderr, "--independent") {
		t.Errorf("stderr %q: want a diagnostic naming the closed descriptor and --independent", r.stderr)
	}
}

func TestIndependentQueues(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 2\n")
	r := outer(t, dir, []string{"cpu=2"}, `"$BIN" run --independent --timeout 1s -- true; echo "code=$?"`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if strings.TrimSpace(r.stdout) != "code=75" || !strings.Contains(r.stderr, "not admitted within 1s") {
		t.Errorf("stdout %q stderr %q: an --independent run must queue behind its parent", r.stdout, r.stderr)
	}
}

func TestIndependentDropsInheritedContext(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	r := outer(t, dir, []string{"cpu=2"}, `exec 7</dev/null; "$BIN" run --independent --need cpu=1 -- $FDS`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	env := envOf(r.stdout)
	if env["CORESLOT_TOKEN_FDS"] != "4" || env["CORESLOT_GRANTED_CPU"] != "1" || env["CORESLOT_HELD_FD"] != "3" {
		t.Errorf("the command's environment %v: want only its own grant", env)
	}
	fds := fdsOf(r.stdout)
	if fds[4] != inode(t, filepath.Join(dir, "res", "cpu", "2")) {
		t.Errorf("fd 4 is %q, want its own token cpu/2", fds[4])
	}
	// The helper's own runtime may open descriptors of its own, so check identities, not numbers.
	for i := 0; i < 2; i++ {
		parent := inode(t, filepath.Join(dir, "res", "cpu", strconv.Itoa(i)))
		for fd, id := range fds {
			if id == parent {
				t.Errorf("fd %d is the parent's token cpu/%d, which was passed on: %v", fd, i, fds)
			}
		}
	}
	if fds[7] != inode(t, "/dev/null") {
		t.Errorf("fd 7, an unrelated inherited descriptor, was not passed through: %v", fds)
	}
}

func TestNestedUnderUnbudgetedParent(t *testing.T) {
	show := `echo "$CORESLOT_GRANTED_CPU $CORESLOT_UNBUDGETED"`

	t.Run("fresh directory", func(t *testing.T) {
		dir := stateDir(t, "")
		env := append(baseEnv(dir), "CORESLOT_UNBUDGETED=parent-reason", "CORESLOT_GRANTED_CPU=5")
		r := cli(t, env, "run", "--need", "cpu=3..6", "--", "sh", "-c", show)
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		if got := strings.TrimSpace(r.stdout); got != "3 parent-reason" {
			t.Errorf("the command saw %q, want its min 3 and the parent's reason", got)
		}
		if !strings.Contains(r.stderr, "unbudgeted") {
			t.Errorf("stderr %q: want a notice", r.stderr)
		}
		if names := entries(t, dir, ""); len(names) != 0 {
			t.Errorf("an unbudgeted pass-through wrote %q", names)
		}
	})

	t.Run("format understood", func(t *testing.T) {
		dir := stateDir(t, "")
		if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
			t.Fatalf("setup run: %d %q", r.code, r.stderr)
		}
		gen, _ := os.ReadFile(filepath.Join(dir, "gen"))
		env := append(baseEnv(dir), "CORESLOT_UNBUDGETED=parent-reason")
		if r := cli(t, env, "run", "--need", "cpu=3", "--label", "nested-unbudgeted", "--", "true"); r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		if after, _ := os.ReadFile(filepath.Join(dir, "gen")); string(after) != string(gen) {
			t.Errorf("gen changed: %q -> %q", gen, after)
		}
		if _, err := os.Stat(filepath.Join(dir, "degraded")); err == nil {
			t.Error("the pass-through wrote a degraded record")
		}
		lines := nestedLines(t, dir)
		if len(lines) != 1 || lines[0]["label"] != "nested-unbudgeted" || fmt.Sprint(lines[0]["unbudgeted_resources"]) != "[cpu]" {
			t.Errorf("nested run-log lines %v", lines)
		}
	})
}

func TestValidateInherited(t *testing.T) {
	dir := stateDir(t, "")
	res := filepath.Join(dir, "res", "cpu")
	if err := os.MkdirAll(res, 0o700); err != nil {
		t.Fatal(err)
	}
	// Tokens 0 and 1 are locked through descriptors this process holds; token 2 exists unlocked.
	var tokens []*os.File
	for i := 0; i < 3; i++ {
		f, err := os.OpenFile(filepath.Join(res, strconv.Itoa(i)), os.O_RDONLY|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		if i < 2 {
			if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
		}
		tokens = append(tokens, f)
	}
	// A validated descriptor is owned by the *os.File ValidateInherited returns, which closes it
	// when collected: each call gets fresh duplicates, and only one owner ever closes them.
	dup := func(f *os.File, owned bool) int {
		fd, err := unix.Dup(int(f.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		if !owned {
			t.Cleanup(func() { unix.Close(fd) })
		}
		return fd
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devnull.Close() })
	closed := 1000
	for _, err := unix.FcntlInt(uintptr(closed), unix.F_GETFD, 0); err == nil; _, err = unix.FcntlInt(uintptr(closed), unix.F_GETFD, 0) {
		closed++
	}
	list := func(xs ...int) string {
		var s []string
		for _, x := range xs {
			s = append(s, strconv.Itoa(x))
		}
		return strings.Join(s, ",")
	}
	check := func(tokenFDs string) (map[string][]*os.File, error) {
		return engine.ValidateInherited(map[string]string{"CORESLOT_STATE_DIR": dir, "CORESLOT_TOKEN_FDS": tokenFDs})
	}

	t0, t1, t2 := dup(tokens[0], false), dup(tokens[1], false), dup(tokens[2], false)
	for name, v := range map[string]string{
		"listed twice":      list(t0, t0),
		"overlapping range": fmt.Sprintf("%d,%d-%d", t1, min(t0, t1), max(t0, t1)),
		"not open":          list(t0, closed),
		"not a token":       list(int(devnull.Fd())),
		"token not locked":  list(t2),
		"malformed":         "4-",
		"descending range":  "9-4",
		"more than 512":     "3-515",
		"sign":              "+4",
	} {
		if held, err := check(v); err == nil {
			t.Errorf("%s (%q) validated: %v", name, v, held)
		}
	}
	if _, err := engine.ValidateInherited(map[string]string{"CORESLOT_STATE_DIR": filepath.Join(dir, "missing"),
		"CORESLOT_TOKEN_FDS": list(t0)}); err == nil {
		t.Error("a context whose state directory cannot be read validated")
	}

	held, err := check(list(dup(tokens[0], true), dup(tokens[1], true)))
	if err != nil || len(held["cpu"]) != 2 {
		t.Fatalf("two locked tokens: held %v, err %v", held, err)
	}
	for _, f := range held["cpu"] {
		f.Close()
	}
	one := dup(tokens[0], true)
	held, err = check(fmt.Sprintf("%d-%d", one, one))
	if err != nil || len(held["cpu"]) != 1 {
		t.Fatalf("a single descriptor written as a range: held %v, err %v", held, err)
	}
	held["cpu"][0].Close()
}
