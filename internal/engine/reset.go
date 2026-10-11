//go:build unix

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/wire"
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

// Reset removes the contents of the state directory except the format-stable files and the
// configuration (`coreslot doctor --reset`): inside the admission section it
// checks that there are no holds, no tickets, no other section holder and no locked tokens, and
// only then removes the entries of queue/, holds/, cancel/ and log/, gen, FORMAT and degraded,
// keeping admit.lock, res/ and config.toml. It returns the number of entries removed.
//
// When something is alive it removes nothing and returns a *LiveError naming it. A section that
// ctx does not let it enter is one more live thing: another client is inside it. The state is
// not format-checked, so a malformed FORMAT is repaired by a reset like any other.
func (e *Engine) Reset(ctx context.Context) (removed int, err error) {
	sec, err := enterSection(e.st, ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, &LiveError{Reasons: []string{e.sectionHolderReason()}}
		}
		return 0, err
	}
	defer sec.Leave()
	live, removed, err := sec.Reset()
	if err != nil {
		return removed, err
	}
	if !live.Empty() {
		return 0, &LiveError{Reasons: liveReasons(live)}
	}
	return removed, nil
}

// sectionHolderReason names the client inside the section from admit.lock's informational
// body, which may be stale or unreadable.
func (e *Engine) sectionHolderReason() string {
	body, err := e.readBody("", store.AdmitLock)
	if err == nil {
		var b struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(body, &b) == nil && b.PID > 0 {
			return fmt.Sprintf("another client is inside the admission section (admit.lock records pid %d)", b.PID)
		}
	}
	return "another client is inside the admission section"
}

func liveReasons(l store.LiveState) []string {
	var out []string
	add := func(kind string, names []string) {
		if len(names) == 0 {
			return
		}
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = entryText(n)
		}
		out = append(out, fmt.Sprintf("%d live %s (%s)", len(names), plural(kind, len(names)), strings.Join(parts, ", ")))
	}
	add("hold", l.Holds)
	add("ticket", l.Tickets)
	if len(l.Tokens) > 0 {
		var parts []string
		for _, r := range slices.Sorted(maps.Keys(l.Tokens)) {
			parts = append(parts, fmt.Sprintf("%s %d", r, l.Tokens[r]))
		}
		out = append(out, "locked tokens ("+strings.Join(parts, ", ")+")")
	}
	return out
}

func plural(kind string, n int) string {
	if n == 1 {
		return kind
	}
	return kind + "s"
}

// entryText is "pid N" and the display of a ticket or hold name, both safe to print: a name
// only holds [A-Za-z0-9._-] and digits.
func entryText(name string) string {
	id, ok := wire.ParseName(name)
	if !ok {
		return name
	}
	const idLen = 20 + 1 + 10 + 1 + 6
	if len(name) > idLen+1 {
		return fmt.Sprintf("pid %d, %s", id.PID, name[idLen+1:])
	}
	return fmt.Sprintf("pid %d", id.PID)
}
