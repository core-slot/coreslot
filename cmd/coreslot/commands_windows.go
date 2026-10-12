//go:build windows

package main

// Windows has no budget (PROTOCOL.md section 16): there is nothing to inspect, cancel or repair,
// so these commands only answer a request for help (noBudget).

func statusCommand(args []string, io IO) int {
	fs, _, _ := statusFlags()
	return noBudget("status", statusUsage, fs, args, io)
}

func cancelCommand(args []string, io IO) int {
	fs, _ := cancelFlags()
	return noBudget("cancel", cancelUsage, fs, args, io)
}

func doctorCommand(args []string, io IO) int {
	fs, _, _ := doctorFlags()
	return noBudget("doctor", doctorUsage, fs, args, io)
}
