//go:build coreslot_testhooks && unix

package conformance

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/store"
)

// trimDoc is the part of `status --json` a trim trace reads.
type trimDoc struct {
	Mem struct {
		ReservedMiB int64 `json:"reserved_mib"`
		RoomMiB     int64 `json:"room_mib"`
	} `json:"mem"`
	Holds []trimDocHold `json:"holds"`
}

type trimDocHold struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	NeedMiB      int64  `json:"need_mib"`
	AskedMiB     int64  `json:"asked_mib"`
	FootprintMiB int64  `json:"footprint_mib"`
	Trimmed      bool   `json:"trimmed"`
}

// statusDoc runs `status --json` and decodes it.
func (e Env) statusDoc() trimDoc {
	e.t.Helper()
	p := e.Start("status", "--json")
	p.ExitCode(0)
	var d trimDoc
	if err := json.Unmarshal([]byte(p.Stdout()), &d); err != nil {
		e.t.Fatalf("status --json: %v\n%s", err, p.Stdout())
	}
	return d
}

// hold is the hold labelled label in d.
func (d trimDoc) hold(t *testing.T, label string) trimDocHold {
	t.Helper()
	for _, h := range d.Holds {
		if h.Label == label {
			return h
		}
	}
	t.Fatalf("status --json has no hold %s: %+v", label, d.Holds)
	return trimDocHold{}
}

// growth is what a hold takes from the room: what it reserves above what it uses.
func (h trimDocHold) growth() int64 { return max(0, h.NeedMiB-h.FootprintMiB) }

// A run that asked for far more than its command uses keeps a waiter out; `coreslot trim` with the
// id status prints makes that run lower its reservation to what its command uses plus the margin,
// and the room it gives back lets the waiter in. Mutation: make the run ignore the request
// (watchChild never reads trim.json) → trim says the run has not answered, and the waiter never
// starts, red.
func TestTraceTrimAdmitsWaiter(t *testing.T) {
	e := State(t)
	const roomMiB, bigMiB, waiterMiB = 6000, 4096, 3072 // the two needs do not fit together
	e.WriteConfig(roomConfig(roomMiB) + telemetrySwitch + "[watch]\ntrim_window_s = 2\n")

	big := e.Run("big", "--mem", "4G")
	e.WaitStarted("big")
	e.WaitTicked("big")
	waiter := e.Run("waiter", "--mem", "3G")
	e.Eventually(startWait, "the waiter says it waits", func() bool { return strings.Contains(waiter.Stderr(), "waiting for") })
	var reason any
	for _, m := range jsonLines(t, filepath.Join(e.Dir, store.TelemetryDir, store.EventsFile)) {
		if m["e"] == "waiting" && m["label"] == "waiter" {
			reason = m["reason"]
		}
	}
	if reason != "room_short" {
		t.Fatalf("the waiter's waiting event has reason %v, want room_short", reason)
	}

	before := e.statusDoc()
	b0 := before.hold(t, "big")
	if b0.Trimmed || b0.NeedMiB != bigMiB || b0.AskedMiB != bigMiB || b0.ID == "" {
		t.Fatalf("before the trim: %+v; want need and asked %d, not trimmed, an id", b0, bigMiB)
	}
	// The run's window spans two 1 s readings: once the hold is older than that, a trim lowers it
	// at the run's next tick.
	e.Eventually(startWait, "the hold outlives its window", func() bool {
		h, ok := e.Hold("big")
		return ok && time.Duration(store.NowNS()-h.StartNS) > 3*time.Second
	})
	if _, ok := e.Started("waiter"); ok {
		t.Fatal("the waiter started before the trim")
	}

	tr := e.Start("trim", b0.ID)
	tr.ExitCode(0)
	e.WaitStarted("waiter")
	if out := tr.Stdout(); !strings.HasPrefix(out, "coreslot: big reserves ") || !strings.HasSuffix(out, " now (asked 4 GiB) and follows what it uses\n") {
		t.Errorf("trim printed %q (stderr %q); want that big's reservation was lowered", out, tr.Stderr())
	}

	st := e.Start("status")
	st.ExitCode(0)
	if !strings.Contains(st.Stdout(), "  trimmed  id "+b0.ID+"\n") {
		t.Errorf("status does not show big as trimmed, with its id:\n%s", st.Stdout())
	}
	t.Logf("trim: %sstatus:\n%s", tr.Stdout(), st.Stdout())

	after := e.statusDoc()
	b1, w1 := after.hold(t, "big"), after.hold(t, "waiter")
	if !b1.Trimmed || b1.AskedMiB != bigMiB || b1.NeedMiB >= bigMiB {
		t.Errorf("big after the trim: %+v; want trimmed, asked_mib %d, need_mib below it", b1, bigMiB)
	}
	if w1.Trimmed || w1.AskedMiB != waiterMiB || w1.NeedMiB != waiterMiB {
		t.Errorf("the waiter: %+v; want need and asked %d, not trimmed", w1, waiterMiB)
	}
	if after.Mem.ReservedMiB != b1.NeedMiB+w1.NeedMiB {
		t.Errorf("reserved_mib %d, want the holds' need_mib %d + %d", after.Mem.ReservedMiB, b1.NeedMiB, w1.NeedMiB)
	}
	// The room rose by what big gave back, less what the waiter now takes.
	if rise, want := after.Mem.RoomMiB-before.Mem.RoomMiB, b0.growth()-b1.growth()-w1.growth(); rise != want {
		t.Errorf("room_mib %d → %d: rose %d, want %d (big %+v → %+v, waiter %+v)",
			before.Mem.RoomMiB, after.Mem.RoomMiB, rise, want, b0, b1, w1)
	}

	e.ReleaseChild("waiter")
	waiter.ExitCode(0)
	e.ReleaseChild("big")
	big.ExitCode(0)
}
