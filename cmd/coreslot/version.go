package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Nezhinskiy/coreslot/internal/testhooks"
)

// versionCommand prints the CLI's version and the protocol formats it implements (spec section
// 6.2), for example `coreslot 0.1.0 (protocol formats: 1)`.
func versionCommand(args []string, io IO) int {
	if len(args) > 0 {
		fmt.Fprintf(io.Stderr, "coreslot: version takes no arguments\n")
		return exitUsage
	}
	formats := testhooks.Formats()
	text := make([]string, len(formats))
	for i, f := range formats {
		text[i] = strconv.Itoa(f)
	}
	fmt.Fprintf(io.Stdout, "coreslot %s (protocol formats: %s)\n", version, strings.Join(text, ", "))
	return 0
}
