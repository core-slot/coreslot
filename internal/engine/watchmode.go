//go:build unix

package engine

import "context"

// SetWatchModeContext sets this machine's override of the watchdog's mode inside the section, entered
// within ctx (PROTOCOL.md section 7.15): mode "observe" or "enforce" until untilNS on the host's wall
// clock (0: until removed), or "" to remove it. A directory of a format this client does not
// implement is refused.
func (e *Engine) SetWatchModeContext(ctx context.Context, mode string, untilNS int64) error {
	sec, err := e.enterUntil(ctx)
	if err != nil {
		return err
	}
	defer sec.Leave()
	n, understood, err := sec.Prepare(e.clock.Realtime())
	if err != nil {
		return err
	}
	if !understood {
		return refuseFormat(n, "")
	}
	if mode == "" {
		return sec.RemoveWatchMode()
	}
	return sec.SetWatchMode(mode, untilNS)
}
