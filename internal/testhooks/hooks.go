//go:build !coreslot_testhooks

// Package testhooks provides pause points, a wall-clock offset, a format list and a getent
// substitute for the deterministic trace tests and the password-database fallback test. They act only in binaries built with the coreslot_testhooks tag;
// in every other build they are inert and ignore the environment.
package testhooks

import "time"

// Point is a named pause point; inert without the coreslot_testhooks tag.
func Point(name string) {}

// RealtimeOffset is added to the wall clock; always 0 without the coreslot_testhooks tag.
func RealtimeOffset() time.Duration { return 0 }

// Formats lists the formats the client implements; always [1] without the coreslot_testhooks tag.
func Formats() []int { return []int{1} }

// Getent names a getent to use instead of the system's; always "" without the coreslot_testhooks
// tag, so that no environment of a release build can choose the program the home directory comes
// from.
func Getent() string { return "" }
