//go:build coreslot_testhooks && unix

package conformance

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/store"
)

// A head that does not fit waits, and a ticket behind it waits behind it; `coreslot start` on the
// second admits it at once with its full need, the room goes below zero, and the head keeps
// waiting until the memory frees up. Mutation: make the run ignore its start request (Step never
// checks start/) → start says the run has not answered, and the ticket behind never starts, red.
func TestTraceStartBehindTheHead(t *testing.T) {
	e := State(t)
	e.WriteConfig(roomConfig(6000) + telemetrySwitch)

	big := e.Run("big", "--mem", "4G")
	e.WaitStarted("big")
	head := e.Run("head", "--mem", "3G")
	e.Eventually(startWait, "the head says it waits", func() bool { return strings.Contains(head.Stderr(), "waiting for") })
	behind := e.Run("behind", "--mem", "2G")
	e.Eventually(startWait, "the ticket behind says it waits", func() bool { return strings.Contains(behind.Stderr(), "waiting for") })

	st := e.Start("start", "--label", "behind")
	st.ExitCode(0)
	e.WaitStarted("behind")
	if out := st.Stdout(); out != "coreslot: behind started now, out of turn; its full need (2 GiB) still counts\n" {
		t.Errorf("start printed %q (stderr %q)", out, st.Stderr())
	}
	d := e.statusDoc()
	if b := d.hold(t, "behind"); !b.Forced || b.NeedMiB != 2048 {
		t.Errorf("the started hold %+v; want forced with its full 2048 MiB", b)
	}
	if d.Mem.RoomMiB >= 0 {
		t.Errorf("room_mib %d after the forced start, want below 0", d.Mem.RoomMiB)
	}
	if _, ok := e.Started("head"); ok {
		t.Error("the head started beside the forced start")
	}
	pass := ""
	for _, m := range jsonLines(t, filepath.Join(e.Dir, store.TelemetryDir, store.EventsFile)) {
		if m["e"] == "admitted" && m["label"] == "behind" {
			pass, _ = m["pass"].(string)
		}
	}
	if pass != "forced" {
		t.Errorf("the admitted event of behind has pass %q, want forced", pass)
	}

	e.ReleaseChild("behind")
	behind.ExitCode(0)
	e.ReleaseChild("big")
	big.ExitCode(0)
	e.WaitStarted("head")
	e.ReleaseChild("head")
	head.ExitCode(0)
}
