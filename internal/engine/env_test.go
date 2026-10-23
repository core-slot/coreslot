package engine

import (
	"strings"
	"testing"
)

// ChildEnv is the one builder of a command's CORESLOT_* environment (PROTOCOL.md section 17): an
// admitted grant, a token-only grant, a fail-open grant with and without its hold, a timeout
// run, a pass-through's narrowed grant and a Windows run all get their variables from it, so
// each case pins the bytes those callers produced before they shared it. Mutation: sorting by
// environment form instead of resource name reddens "resource-name order"; dropping the case of a
// single token reddens "one token"; writing mem's token count reddens "mem in MiB".
//
// The order is the resource name's, as the descriptors' is: CORESLOT_GRANTED_A_B sorts before
// CORESLOT_GRANTED_A0 although '_' sorts after '0'. The CLI's pass-through used to order the
// variables it sets by their environment form, and its Windows run by map iteration; the two
// orders differ only for names such as "a-b" and "a0", and no reader of the environment sees the
// order of its variables.
func TestChildEnv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		granted    map[string]int
		held       bool
		tokens     int
		unbudgeted string
		want       string
	}{
		{name: "admitted", granted: map[string]int{"cpu": 3, "pg-local": 1}, held: true, tokens: 4,
			want: "CORESLOT_GRANTED_CPU=3 CORESLOT_GRANTED_PG_LOCAL=1 CORESLOT_HELD_FD=3 CORESLOT_TOKEN_FDS=4-7"},
		{name: "one token", granted: map[string]int{"cpu": 1}, held: true, tokens: 1,
			want: "CORESLOT_GRANTED_CPU=1 CORESLOT_HELD_FD=3 CORESLOT_TOKEN_FDS=4"},
		{name: "token-only", granted: map[string]int{"cpu": 2}, tokens: 2, unbudgeted: "format",
			want: "CORESLOT_GRANTED_CPU=2 CORESLOT_TOKEN_FDS=4-5 CORESLOT_UNBUDGETED=format"},
		{name: "fail-open with its hold", granted: map[string]int{"cpu": 2, "gpu": 1}, held: true, unbudgeted: "state dir gone",
			want: "CORESLOT_GRANTED_CPU=2 CORESLOT_GRANTED_GPU=1 CORESLOT_HELD_FD=3 CORESLOT_UNBUDGETED=state dir gone"},
		{name: "fail-open without a hold", granted: map[string]int{"cpu": 2}, unbudgeted: "timeout",
			want: "CORESLOT_GRANTED_CPU=2 CORESLOT_UNBUDGETED=timeout"},
		{name: "granted only", granted: map[string]int{"pg-local": 1, "cpu": 4},
			want: "CORESLOT_GRANTED_CPU=4 CORESLOT_GRANTED_PG_LOCAL=1"},
		{name: "resource-name order", granted: map[string]int{"ab": 1, "a-b": 2, "a0": 3},
			want: "CORESLOT_GRANTED_A_B=2 CORESLOT_GRANTED_A0=3 CORESLOT_GRANTED_AB=1"},
		{name: "mem in MiB", granted: map[string]int{"cpu": 2, "mem": 24},
			want: "CORESLOT_GRANTED_CPU=2 CORESLOT_GRANTED_MEM=3072"},
		{name: "nothing", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(ChildEnv(tc.granted, tc.held, tc.tokens, tc.unbudgeted), " ")
			if got != tc.want {
				t.Errorf("ChildEnv = %q\n               want %q", got, tc.want)
			}
		})
	}
}
