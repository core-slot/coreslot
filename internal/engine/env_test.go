package engine

import (
	"strings"
	"testing"
)

// ChildEnv is the one builder of a command's CORESLOT_* environment (PROTOCOL.md section 17): an
// admitted grant, a fail-open grant with and without its hold, a timeout run, a nested run's
// narrowed grant and a Windows run all get their variables from it, so each case pins the bytes.
// Mutation: dropping CORESLOT_HOLD reddens "admitted".
func TestChildEnv(t *testing.T) {
	const hold = "01791720600000000000-0000004711-000001-gate"
	for _, tc := range []struct {
		name             string
		grantMiB         int64
		hold, unbudgeted string
		want             string
	}{
		{name: "admitted", grantMiB: 3072, hold: hold,
			want: "CORESLOT_GRANTED_MEM=3072 CORESLOT_HELD_FD=3 CORESLOT_HOLD=" + hold},
		{name: "fail-open with its hold", grantMiB: 256, hold: hold, unbudgeted: "state dir gone",
			want: "CORESLOT_GRANTED_MEM=256 CORESLOT_HELD_FD=3 CORESLOT_HOLD=" + hold + " CORESLOT_UNBUDGETED=state dir gone"},
		{name: "fail-open without a hold", grantMiB: 256, unbudgeted: "timeout",
			want: "CORESLOT_GRANTED_MEM=256 CORESLOT_UNBUDGETED=timeout"},
		{name: "granted only", grantMiB: 40960, want: "CORESLOT_GRANTED_MEM=40960"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(ChildEnv(tc.grantMiB, tc.hold, tc.unbudgeted), " ")
			if got != tc.want {
				t.Errorf("ChildEnv = %q\n               want %q", got, tc.want)
			}
		})
	}
}
