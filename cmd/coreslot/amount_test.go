package main

import "testing"

// A mem amount reads in bytes, any other resource's in its tokens. Mutation: amount printing the
// token count for mem turns the mem rows red.
func TestAmount(t *testing.T) {
	for _, c := range []struct {
		name   string
		tokens int
		want   string
	}{
		{"cpu", 12, "12"},
		{"pg-local", 1, "1"},
		{"mem", 24, "3 GiB"},
		{"mem", 76, "9.5 GiB"},
		{"mem", 1, "128 MiB"},
	} {
		if got := amount(c.name, c.tokens); got != c.want {
			t.Errorf("amount(%q, %d) = %q, want %q", c.name, c.tokens, got, c.want)
		}
	}
}
