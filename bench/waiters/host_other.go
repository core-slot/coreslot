//go:build coreslot_bench && unix && !darwin && !linux

package main

import "runtime"

func host() hostInfo {
	return hostInfo{Model: "unknown", CPUs: runtime.NumCPU(), OS: runtime.GOOS}
}

func loadAvg() string { return "unavailable" }
