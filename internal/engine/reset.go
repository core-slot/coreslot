//go:build unix

package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// LiveError is the refusal of Reset: something is alive, so nothing was removed. It matches
// ErrRefused. Reasons name each live thing, one per element, for the caller to print.
type LiveError struct {
	Reasons []string
}

func (e *LiveError) Error() string {
	return "coreslot: refused: " + strings.Join(e.Reasons, "; ")
}

func (e *LiveError) Unwrap() error { return ErrRefused }

// Reset recreates the state directory as the format this client writes (`coreslot doctor
// --reset`), whatever format it had: inside the admission section it checks that there are no
// holds, no tickets and no other section holder, and only then removes the entries of queue/,
// holds/, cancel/, log/ unless store.ResetKeepsLog, gen, degraded and another format's res/, and
// writes FORMAT, keeping admit.lock and config.toml. It returns the entries removed and keptLog.
//
// When something is alive it removes nothing and returns a *LiveError naming it. A section that
// ctx does not let it enter is one more live thing: another client is inside it. The state is
// not format-checked, so a malformed or foreign FORMAT is replaced by a reset like any other.
func (e *Engine) Reset(ctx context.Context) (removed int, keptLog bool, err error) {
	sec, err := enterSection(e.st, ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, false, &LiveError{Reasons: []string{e.sectionHolderReason()}}
		}
		return 0, false, err
	}
	defer sec.Leave()
	keptLog = e.st.ResetKeepsLog()
	live, removed, err := sec.Reset()
	if err != nil {
		return removed, keptLog, err
	}
	if !live.Empty() {
		return 0, false, &LiveError{Reasons: e.liveReasons(live)}
	}
	return removed, keptLog, nil
}

// sectionHolderReason names the client inside the section from admit.lock's informational
// body, which may be stale or unreadable.
func (e *Engine) sectionHolderReason() string {
	if a, ok := e.sectionHolder(); ok {
		return fmt.Sprintf("another client is inside the admission section (admit.lock records pid %d)", a.PID)
	}
	return "another client is inside the admission section"
}

func (e *Engine) liveReasons(l store.LiveState) []string {
	var out []string
	add := func(kind, dir string, names []string) {
		if len(names) == 0 {
			return
		}
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = e.entryText(dir, n)
		}
		out = append(out, fmt.Sprintf("%d live %s (%s)", len(names), plural(kind, len(names)), strings.Join(parts, ", ")))
	}
	add("hold", store.HoldsDir, l.Holds)
	add("ticket", store.QueueDir, l.Tickets)
	return out
}

func plural(kind string, n int) string {
	if n == 1 {
		return kind
	}
	return kind + "s"
}

// entryText names a live ticket or hold of dir for a person: "pid N" and its label from the
// body, escaped for a terminal (wire.EscapeHuman). Without a readable body or label it falls back
// to the display part of the name, which only holds [A-Za-z0-9._] and is safe to print.
func (e *Engine) entryText(dir, name string) string {
	id, ok := wire.ParseName(name)
	if !ok {
		return name
	}
	label := ""
	if body, err := e.readBody(dir, name); err == nil {
		if dir == store.HoldsDir {
			if h, err := wire.DecodeHold(body); err == nil {
				label = h.Label
			}
		} else if tk, err := wire.DecodeTicket(body); err == nil {
			label = tk.Label
		}
	}
	if label != "" {
		return fmt.Sprintf("pid %d, %s", id.PID, wire.EscapeHuman(label))
	}
	if d := wire.NameDisplay(name); d != "" {
		return fmt.Sprintf("pid %d, %s", id.PID, d)
	}
	return fmt.Sprintf("pid %d", id.PID)
}
