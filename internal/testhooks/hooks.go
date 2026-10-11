//go:build !coreslot_testhooks

// Package testhooks provides pause points, a wall-clock offset and a format list for the
// deterministic trace tests. They act only in binaries built with the coreslot_testhooks tag;
// in every other build they are inert and ignore the environment.
package testhooks

import "time"

// Point is a named pause point; inert without the coreslot_testhooks tag.
func Point(name string) {}

// RealtimeOffset is added to the wall clock; always 0 without the coreslot_testhooks tag.
func RealtimeOffset() time.Duration { return 0 }

// Formats lists the formats the client implements; always [1] without the coreslot_testhooks tag.
func Formats() []int { return []int{1} }
