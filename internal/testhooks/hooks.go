//go:build !coreslot_testhooks

// Package testhooks provides pause points, a wall-clock offset, a format list, a getent substitute,
// a password database that finds no record, a memory headroom and machine readings for the
// deterministic trace tests and the fallback tests.
// They act only in binaries built with the coreslot_testhooks tag; in every other build they are
// inert and ignore the environment.
package testhooks

import "time"

// Point is a named pause point; inert without the coreslot_testhooks tag.
func Point(name string) {}

// RealtimeOffset is added to the wall clock; always 0 without the coreslot_testhooks tag.
func RealtimeOffset() time.Duration { return 0 }

// Getent names a getent to use instead of the system's; always "" without the coreslot_testhooks
// tag, so that no environment of a release build can choose the program the home directory comes
// from.
func Getent() string { return "" }

// PasswdMiss reports whether the home directory's password-database lookup is to find no record,
// so that its NSS fallback runs; always false without the coreslot_testhooks tag.
func PasswdMiss() bool { return false }

// Headroom replaces the kernel's memory headroom, in MiB; always 0, false without the
// coreslot_testhooks tag.
func Headroom() (mib int64, set bool) { return 0, false }

// Readings replaces machine readings by name; always nil, false without the coreslot_testhooks
// tag.
func Readings() (nums map[string]int64, set bool) { return nil, false }
