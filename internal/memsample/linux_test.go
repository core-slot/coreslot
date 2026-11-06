//go:build linux

package memsample

import (
	"slices"
	"testing"
)

// A name may hold spaces and either parenthesis, so the fields start after the last ')'. Mutation: the
// first ')' for the last → "a) (b" is cut short and its fields shift, red.
func TestParseStat(t *testing.T) {
	name, f, ok := parseStat("42 (a) (b c) S 7 42 42\n")
	if !ok || name != "a) (b c" || !slices.Equal(f, []string{"S", "7", "42", "42"}) {
		t.Errorf("parseStat = %q, %q, %v", name, f, ok)
	}
	if _, _, ok := parseStat("42 no name"); ok {
		t.Errorf("parseStat(no parenthesis) ok, want not")
	}
}
