//go:build unix

package main

import (
	"fmt"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
)

// Bounds of the memory hint: a run shorter than memHintAfter says nothing, a peak within memHintSlack
// of its grant is none, and a peak with no mem need is worth one only above memHintAlone.
const (
	memHintAfter = time.Second
	memHintAlone = 1024 // MiB
	memHintSlack = 128  // MiB
)

// memHint is the line saying that a run's memory peak, peakKiB over ranS seconds, outgrew its mem
// grant of grantMiB by more than memHintSlack, or passed memHintAlone with no mem grant, or "" when
// it did not. A grant the budget clamped is named as the cause, since a bigger need would not
// change it; otherwise the line points at stats for the label. A run without
// a label is never learned, so it gets no hint.
func memHint(label string, grantMiB int64, clamped bool, peakKiB int64, ranS float64) string {
	peakMiB := peakKiB / 1024
	if label == "" || ranS < memHintAfter.Seconds() {
		return ""
	}
	peak := spec.FormatMiB(peakMiB)
	stats := "coreslot stats --label " + label
	switch {
	case grantMiB == 0 && peakMiB > memHintAlone:
		return fmt.Sprintf("%s peaked at %s with no mem need; %s", label, peak, stats)
	case grantMiB == 0 || peakMiB <= grantMiB+memHintSlack:
		return ""
	case clamped:
		return fmt.Sprintf("%s peaked at %s on a mem grant clamped to the budget's %s", label, peak, spec.FormatMiB(grantMiB))
	}
	return fmt.Sprintf("%s peaked at %s on a %s mem grant; %s", label, peak, spec.FormatMiB(grantMiB), stats)
}
