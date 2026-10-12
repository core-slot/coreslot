//go:build windows

package main

import "fmt"

// Windows has no budget (PROTOCOL.md section 16): there is nothing to inspect, cancel or repair.

func statusCommand(args []string, io IO) int { return noBudget("status", io) }
func cancelCommand(args []string, io IO) int { return noBudget("cancel", io) }
func doctorCommand(args []string, io IO) int { return noBudget("doctor", io) }

func noBudget(name string, io IO) int {
	fmt.Fprintf(io.Stderr, "coreslot: %s: Windows has no coreslot budget\n", name)
	return exitInternal
}
