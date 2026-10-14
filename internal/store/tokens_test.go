//go:build unix

package store

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/wire"
)

func tokenPath(dir, res string, i int) string {
	return filepath.Join(dir, ResDir, res, fmt.Sprint(i))
}

func take(t *testing.T, sec *Section, want, sizes map[string]int) (*TokenSet, map[string]int) {
	t.Helper()
	ts, got, err := sec.TakeTokens(want, sizes)
	if err != nil {
		t.Fatalf("TakeTokens(%v, %v): %v", want, sizes, err)
	}
	t.Cleanup(ts.Close)
	return ts, got
}

func countLocked(t *testing.T, sec *Section, res string) int {
	t.Helper()
	n, err := sec.CountLocked(res)
	if err != nil {
		t.Fatalf("CountLocked(%s): %v", res, err)
	}
	return n
}

func TestOneOfEightLeavesSeven(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	sizes := map[string]int{"cpu": 8}
	one, got := take(t, sec, map[string]int{"cpu": 1}, sizes)
	if got["cpu"] != 1 || !slices.Equal(one.Indices["cpu"], []int{0}) || len(one.Files) != 1 {
		t.Fatalf("1-token take: got %v, indices %v, %d files", got, one.Indices, len(one.Files))
	}
	if n := countLocked(t, sec, "cpu"); n != 1 {
		t.Errorf("CountLocked after a 1-token take = %d, want 1", n)
	}
	rest, got := take(t, sec, map[string]int{"cpu": 8}, sizes)
	if got["cpu"] != 7 || !slices.Equal(rest.Indices["cpu"], []int{1, 2, 3, 4, 5, 6, 7}) {
		t.Errorf("the rest: got %v, indices %v; want 7 tokens 1-7", got, rest.Indices)
	}
	if n := countLocked(t, sec, "cpu"); n != 8 {
		t.Errorf("CountLocked = %d, want 8", n)
	}
	one.Close()
	if n := countLocked(t, sec, "cpu"); n != 7 {
		t.Errorf("CountLocked after closing the 1-token set = %d, want 7", n)
	}
}

func TestTakeTokensCountsOverSizeHolds(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	// Size 14: create every token file, then a helper holds 10-13 from another process.
	ts, _ := take(t, sec, map[string]int{"cpu": 14}, map[string]int{"cpu": 14})
	ts.Close()
	var high []string
	for i := 10; i <= 13; i++ {
		high = append(high, tokenPath(dir, "cpu", i))
	}
	startHelper(t, "lock", dir, strings.Join(high, "\n"), nil)

	// The size drops to 12: the hold above the new size still counts (I3).
	const size = 12
	locked := countLocked(t, sec, "cpu")
	if locked != 4 {
		t.Fatalf("CountLocked with 10-13 held = %d, want 4 (indices at or above the size count)", locked)
	}
	free := size - locked
	got8, got := take(t, sec, map[string]int{"cpu": free}, map[string]int{"cpu": size})
	if got["cpu"] != 8 || !slices.Equal(got8.Indices["cpu"], []int{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Errorf("take of free=%d: got %v, indices %v; want 0-7", free, got, got8.Indices)
	}
	if n := countLocked(t, sec, "cpu"); n != size {
		t.Errorf("locked after the admission = %d, want the size %d, never above", n, size)
	}
	// Only indices below the size are taken, whatever is asked.
	more, got := take(t, sec, map[string]int{"cpu": 4}, map[string]int{"cpu": size})
	if got["cpu"] != 2 || !slices.Equal(more.Indices["cpu"], []int{8, 9}) {
		t.Errorf("take of 4 more below size 12: got %v, indices %v; want 8, 9", got, more.Indices)
	}
}

func TestTakeTokensSkipsRaceLocked(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	sizes := map[string]int{"cpu": 8, "pg-local": 2}
	// A non-queued taker holds cpu 1 and pg-local 0 without the section's knowledge.
	racer, _ := take(t, sec, map[string]int{"cpu": 2, "pg-local": 1}, sizes)
	racer.Files[0].Close() // keep cpu 1 and pg-local 0 only
	ts, got := take(t, sec, map[string]int{"cpu": 3, "pg-local": 2}, sizes)
	want := map[string]int{"cpu": 3, "pg-local": 1}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if !slices.Equal(ts.Indices["cpu"], []int{0, 2, 3}) || !slices.Equal(ts.Indices["pg-local"], []int{1}) {
		t.Errorf("indices %v, want cpu [0 2 3], pg-local [1]", ts.Indices)
	}
	// Files come in ascending resource name, then index: cpu 0, 2, 3, then pg-local 1.
	var order []string
	for _, f := range ts.Files {
		order = append(order, filepath.Base(filepath.Dir(f.Name()))+"/"+filepath.Base(f.Name()))
	}
	if want := []string{"cpu/0", "cpu/2", "cpu/3", "pg-local/1"}; !slices.Equal(order, want) {
		t.Errorf("file order %v, want %v", order, want)
	}
	if _, _, err := sec.TakeTokens(map[string]int{"../x": 1}, map[string]int{"../x": 1}); err == nil {
		t.Error("TakeTokens accepted a resource name that is not one")
	}
	if _, _, err := sec.TakeTokens(map[string]int{"cpu": 257}, map[string]int{"cpu": 4096}); err == nil {
		t.Error("TakeTokens accepted more than 256 tokens of one resource")
	}
}

func TestCloseDoesNotRevokeChild(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	ts, _ := take(t, sec, map[string]int{"cpu": 2}, map[string]int{"cpu": 8})
	h := startHelper(t, "inherit", dir, "", ts.Files)
	ts.Close()
	ts.Close() // repeated Close only closes
	for i := range 2 {
		if !probeLocked(t, tokenPath(dir, "cpu", i)) {
			t.Errorf("cpu %d was released by the parent's Close while the child holds it", i)
		}
	}
	if n := countLocked(t, sec, "cpu"); n != 2 {
		t.Errorf("CountLocked while the child holds 2 = %d", n)
	}
	h.stop()
	for i := range 2 {
		if probeLocked(t, tokenPath(dir, "cpu", i)) {
			t.Errorf("cpu %d still locked after the child exited", i)
		}
	}
}

func TestInodeMismatchIsDegraded(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	ts, _ := take(t, sec, map[string]int{"cpu": 2}, map[string]int{"cpu": 8})
	if err := ts.CheckIdentity(); err != nil {
		t.Fatalf("CheckIdentity of a fresh grant: %v", err)
	}
	p := tokenPath(dir, "cpu", 1)
	if err := os.Rename(p, p+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wantStateError(t, ts.CheckIdentity(), "CheckIdentity after the token file was replaced")

	ts2, got := take(t, sec, map[string]int{"gpu": 1}, map[string]int{"gpu": 1})
	if got["gpu"] != 1 {
		t.Fatalf("took %v", got)
	}
	if err := os.Remove(tokenPath(dir, "gpu", 0)); err != nil {
		t.Fatal(err)
	}
	wantStateError(t, ts2.CheckIdentity(), "CheckIdentity after the token file was unlinked")
}

func TestProbeAndRemoveHoldKeepsInheritedHold(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	h := wire.Hold{Label: "gate:big", Grant: map[string][]int{"cpu": {0, 1}}, StartNS: 1791720600000000000,
		Clamped: map[string]int{"cpu": 16}}
	f, name, err := sec.PublishHold(h, 7300, 4)
	if err != nil {
		t.Fatal(err)
	}
	if want := wire.HoldName(h.StartNS, 7300, 4, h.Label); name != want {
		t.Errorf("hold name %s, want %s", name, want)
	}
	body, err := os.ReadFile(filepath.Join(dir, HoldsDir, name))
	if err != nil {
		t.Fatal(err)
	}
	if back, err := wire.DecodeHold(body); err != nil || back.Label != h.Label || !slices.Equal(back.Grant["cpu"], []int{0, 1}) {
		t.Errorf("hold body %s decodes to %+v, %v", body, back, err)
	}
	// A second hold with the same name takes the next seq instead of replacing the first.
	f2, name2, err := sec.PublishHold(h, 7300, 4)
	if err != nil || name2 == name {
		t.Fatalf("second PublishHold = %s, %v; want a new name", name2, err)
	}
	f2.Close()

	child := startHelper(t, "inherit", dir, "", []*os.File{f})
	f.Close()
	removed, err := sec.ProbeAndRemoveHold(name)
	if err != nil || removed {
		t.Fatalf("ProbeAndRemoveHold while a child holds the hold = %v, %v; want false", removed, err)
	}
	if !exists(filepath.Join(dir, HoldsDir, name)) {
		t.Fatal("the inherited hold was removed")
	}
	child.stop()
	removed, err = sec.ProbeAndRemoveHold(name)
	if err != nil || !removed || exists(filepath.Join(dir, HoldsDir, name)) {
		t.Errorf("ProbeAndRemoveHold after the child exited = %v, %v", removed, err)
	}
	if removed, err := sec.ProbeAndRemoveHold(name); removed || err != nil {
		t.Errorf("ProbeAndRemoveHold of a removed hold = %v, %v; want false, nil", removed, err)
	}
	if _, err := sec.ProbeAndRemoveHold("../admit.lock"); err == nil {
		t.Error("ProbeAndRemoveHold accepted a name outside the hold grammar")
	}
	if removed, err := sec.ProbeAndRemoveHold(name2); err != nil || !removed {
		t.Errorf("ProbeAndRemoveHold of the closed second hold = %v, %v", removed, err)
	}
}

func TestCountLockedIgnoresForeignNames(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	if n := countLocked(t, sec, "gpu"); n != 0 {
		t.Errorf("CountLocked of a resource with no files = %d", n)
	}
	take(t, sec, map[string]int{"cpu": 1}, map[string]int{"cpu": 1})
	for _, junk := range []string{"01", "x", "4096"} {
		p := filepath.Join(dir, ResDir, "cpu", junk)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		startHelper(t, "lock", dir, p, nil)
	}
	if n := countLocked(t, sec, "cpu"); n != 1 {
		t.Errorf("CountLocked counted files that are not token indices: %d, want 1", n)
	}
	if _, err := sec.CountLocked("CPU"); err == nil {
		t.Error("CountLocked accepted an invalid resource name")
	}
	var se *StateError
	if err := os.Symlink(filepath.Join(dir, AdmitLock), filepath.Join(dir, ResDir, "cpu", "7")); err != nil {
		t.Fatal(err)
	}
	if _, err := sec.CountLocked("cpu"); !errors.As(err, &se) {
		t.Errorf("CountLocked over a symlinked token = %v, want a *StateError", err)
	}
}
