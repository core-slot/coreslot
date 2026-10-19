//go:build unix

package main

import (
	"fmt"
	"math"
	"time"
)

// Bounds of the usage hint, the line a run prints after its command when the cores it used do
// not match the cpu tokens it held. A run shorter than hintAfter says nothing: its average is
// noise and its tokens were not held long enough to matter.
const (
	hintAfter  = time.Minute
	lightBelow = 0.25 // cores: a command this light can run with --light
)

// usageHint is the line comparing the cores a run's command used, cpuS of CPU time over ranS
// seconds of wall clock, with the cpu tokens it held (0 for a --light run, or one that named no
// cpu), or "" when they match. The suggestion is the cores used times 1.2, rounded up, as
// README.md's "Choosing a range" asks. Like the run log, it counts the command and the
// descendants it waited for, not a daemon or a container it talks to.
func usageHint(label string, tokens int, cpuS, ranS float64) string {
	if ranS < hintAfter.Seconds() {
		return ""
	}
	used := cpuS / ranS
	fit := max(1, int(math.Ceil(used*1.2)))
	who := "the command"
	if label != "" {
		who = label
	}
	over := duration(time.Duration(ranS * float64(time.Second)))
	switch {
	case tokens == 0 && used >= 1:
		return fmt.Sprintf("%s used %.2f cpu outside the budget over %s; --need cpu=%d would count it", who, used, over, fit)
	case tokens >= 1 && used < lightBelow:
		return fmt.Sprintf("%s used %.2f of %d cpu over %s; --light runs a command this light without waiting", who, used, tokens, over)
	case tokens >= 2 && used < float64(tokens)/2:
		return fmt.Sprintf("%s used %.2f of %d cpu over %s; --need cpu=%d would fit", who, used, tokens, over, fit)
	case tokens >= 1 && used >= 1.5*float64(tokens) && used-float64(tokens) >= 1:
		return fmt.Sprintf("%s used %.2f cpu on %d over %s; --need cpu=%d would cover it", who, used, tokens, over, fit)
	}
	return ""
}

// hintTokens is the cpu tokens the usage hint compares with, and whether the run gets a hint at
// all: only an admitted run and a --light run sized their own request. A pass-through ran on its
// parent's tokens, and a fail-open or timeout run holds none, whatever it asked for.
func (c child) hintTokens() (int, bool) {
	switch {
	case c.nested:
		return 0, false
	case c.failOpen == lightReason:
		return 0, true
	case c.failOpen != "":
		return 0, false
	}
	return c.grant["cpu"], true
}
