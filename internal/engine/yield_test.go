//go:build unix

package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// A ticket says its run's typical run time, so that a head can tell the short waiters behind it.
// Mutation: the enqueue leaving typical_s out reads 0.
func TestTicketCarriesTypicalS(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	tk := enqueue(t, e, spec.Request{Label: "short", NeedMiB: 64, TypicalS: 90})
	body, err := os.ReadFile(filepath.Join(dir, store.QueueDir, tk.Name()))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := wire.DecodeTicket(body); err != nil || b.TypicalS != 90 {
		t.Errorf("ticket typical_s %d (%v), want 90", b.TypicalS, err)
	}
}
