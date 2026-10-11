//go:build unix && !darwin && !linux

package main

// load1 is the one-minute load average; this platform does not report it here.
func load1() float64 { return 0 }

// fileCap is the most a soft RLIMIT_NOFILE can be set to below the hard limit; 0, none here.
func fileCap() uint64 { return 0 }
