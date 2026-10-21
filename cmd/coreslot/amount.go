package main

import (
	"strconv"

	"github.com/core-slot/coreslot/internal/spec"
)

// amount is tokens of the resource name as a user reads them: a mem amount in bytes (`3 GiB`),
// any other resource's as its count. Machine output (the run log, status --json) keeps tokens.
func amount(name string, tokens int) string {
	if name == "mem" {
		return spec.FormatMiB(int64(tokens) * spec.MemTokenMiB)
	}
	return strconv.Itoa(tokens)
}
