//go:build unix && !coreslot_testhooks

package main

// formatsOfThisBuild is the formats the CLI of this test build implements: format 1 only.
func formatsOfThisBuild() string { return "1" }
