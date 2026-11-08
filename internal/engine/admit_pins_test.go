//go:build unix

package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
)

// unreadableWatch makes watch.json a symbolic link, which the filesystem policy refuses, so that
// ReadWatch returns an error.
func unreadableWatch(t *testing.T, dir string) {
	t.Helper()
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(dir, store.WatchFile)); err != nil {
		t.Fatal(err)
	}
}

// backfillScene is a 1024 MiB job A that has run 20 s of its typical 100, and a head B that needs
// 2048 MiB of the 2048 the machine leaves: B waits for A, so W is 80 s.
func backfillScene(t *testing.T, restartHead bool) (*Engine, string) {
	t.Helper()
	dir := capped(t, 2048)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	hold(t, e, spec.Request{Label: "a", NeedMiB: 1024, TypicalS: 100})
	clk.advance(20 * time.Second)
	enqueue(t, e, spec.Request{Label: "b", NeedMiB: 2048, Restart: restartHead})
	return e, dir
}

// A request whose typical run time equals W passes the head; one second more does not. Mutation:
// `<=` to `<` in passes keeps the first out.
func TestPassesAtExactlyW(t *testing.T) {
	for typical, want := range map[int64]bool{80: true, 81: false} {
		e, _ := backfillScene(t, false)
		got := isAdmitted(step(t, enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: typical})))
		if got != want {
			t.Errorf("typical %d s against W of 80 s: passed %v, want %v", typical, got, want)
		}
	}
}

// A restart head whose watch state cannot be read is not passed, and that is no error of the
// passer. The first request shows the scene lets C pass. Mutation: a ReadWatch error in passes
// returns true, and the passer's own watch read then fails its Step.
func TestPassesRestartHeadUnreadableWatch(t *testing.T) {
	e, dir := backfillScene(t, true)
	admitted(t, step(t, enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 10}))).Close()
	unreadableWatch(t, dir)
	r, err := enqueue(t, e, spec.Request{Label: "d", NeedMiB: 256, TypicalS: 10}).Step(t.Context())
	if err != nil || isAdmitted(r) {
		t.Errorf("Step beside an unreadable watch state = %#v, %v; want waiting with no error", r, err)
	}
}

// With the headroom unknown a restart waits for no margin of room. Mutation: drop m.HeadroomOK
// from the margin clause, where the unread headroom gives no room, and the restart waits.
func TestRestartMarginNotAppliedWhenHeadroomUnknown(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	saved := testHeadroom
	t.Cleanup(func() { testHeadroom = saved })
	testHeadroom.mib, testHeadroom.ok = 0, false // what memsample.Headroom returns unread
	hold(t, e, need(128))
	setWatch(t, e, clk, watch.Detector{CalmSinceNS: sampleWallNS - 11e9}, 0)
	req := need(128)
	req.Restart = true
	admitted(t, step(t, enqueue(t, e, req)))
}

// An error reading the watch state while holds are live is the admission attempt's error: the
// ticket is not admitted. Mutation: admit ignores the error from watchWait and admits.
func TestAdmitReturnsWatchWaitError(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	hold(t, e, need(128))
	unreadableWatch(t, dir)
	r, err := enqueue(t, e, need(128)).Step(t.Context())
	if err == nil || isAdmitted(r) {
		t.Errorf("Step beside an unreadable watch state = %#v, %v; want an error and no admission", r, err)
	}
}

// A restart refused for room makes no tick: watch.json is not written. Mutation: evaluate
// watchWait before the room check, and the restart's own tick writes it.
func TestWatchNotEvaluatedWhenRoomShort(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	hold(t, e, need(128))
	req := need(2048) // the room beside the hold is 896 MiB
	req.Restart = true
	if isAdmitted(step(t, enqueue(t, e, req))) {
		t.Fatal("admitted with too little room: the test shows nothing")
	}
	if _, _, at, err := e.st.ReadWatch(); err != nil || at != 0 {
		t.Errorf("watch.json sampled at %d (%v) by a restart refused for room; want no tick", at, err)
	}
}

// With calm_s at 0 a restart needs no calm, so the level alone holds it, and only a level that
// is Unknown tells it from green here. Mutation: drop d.Level != watch.Green from the restart
// clause, and a fresh Unknown level admits it.
func TestRestartWaitsAtLevelUnknown(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, "[watch]\ncalm_s = 0\n"), clk)
	hold(t, e, need(128))
	req := need(128)
	req.Restart = true
	tk := enqueue(t, e, req)
	setWatch(t, e, clk, watch.Detector{Level: watch.Unknown}, 0)
	if isAdmitted(step(t, tk)) {
		t.Fatal("a restart started at a fresh level that is Unknown")
	}
	clk.advance(time.Second)
	setWatch(t, e, clk, watch.Detector{Level: watch.Green}, 0)
	admitted(t, step(t, tk))
}
