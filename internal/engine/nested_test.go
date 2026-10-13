//go:build unix

package engine

import (
	"slices"
	"testing"

	"github.com/Nezhinskiy/coreslot/internal/config"
)

// stateDirOf reads CORESLOT_STATE_DIR from the context it is given, not from this process's
// environment, and holds it to the rule of PROTOCOL.md section 4.1. Mutation: reading the process
// environment for an unset variable reddens "unset".
func TestStateDirOf(t *testing.T) {
	t.Setenv(config.StateDirEnv, "/process/environment") // not the context's: it must not count
	def, _, err := config.StateDirFrom(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		dir  string
		err  string
	}{
		{name: "unset", env: map[string]string{}, dir: def},
		{name: "empty", env: map[string]string{config.StateDirEnv: ""}, dir: def},
		{name: "absolute", env: map[string]string{config.StateDirEnv: "/srv//state/"}, dir: "/srv/state"},
		{name: "relative", env: map[string]string{config.StateDirEnv: "state"},
			err: `CORESLOT_STATE_DIR="state" is not an absolute path`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := stateDirOf(tc.env)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("stateDirOf(%v) = %q, %v; want the error %q", tc.env, dir, err, tc.err)
				}
				return
			}
			if err != nil || dir != tc.dir {
				t.Fatalf("stateDirOf(%v) = %q, %v; want %q", tc.env, dir, err, tc.dir)
			}
		})
	}
}

// The order of PROTOCOL.md section 18: independent first, then CORESLOT_UNBUDGETED (rule 1), then
// CORESLOT_TOKEN_FDS (rule 2), an empty variable counting as unset. Mutation: checking
// CORESLOT_TOKEN_FDS before CORESLOT_UNBUDGETED reddens "both set"; ignoring independent reddens
// "independent".
func TestDecideNested(t *testing.T) {
	bad := "999999999" // never an open descriptor: the context does not validate
	for _, tc := range []struct {
		name        string
		env         map[string]string
		independent bool
		kind        NestedKind
		err         bool
	}{
		{name: "nothing", env: map[string]string{}, kind: NotNested},
		{name: "empty variables", env: map[string]string{EnvUnbudgeted: "", EnvTokenFDs: ""}, kind: NotNested},
		{name: "unbudgeted", env: map[string]string{EnvUnbudgeted: "why"}, kind: NestedUnbudgeted},
		{name: "both set", env: map[string]string{EnvUnbudgeted: "format", EnvTokenFDs: bad}, kind: NestedUnbudgeted},
		{name: "invalid grant", env: map[string]string{EnvTokenFDs: bad}, err: true},
		{name: "independent", env: map[string]string{EnvUnbudgeted: "why", EnvTokenFDs: bad}, independent: true, kind: NotNested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := DecideNested(tc.env, tc.independent, "")
			if (err != nil) != tc.err || (err == nil && n.Kind != tc.kind) {
				t.Fatalf("DecideNested(%v, %v) = %+v, %v; want kind %d, error %v", tc.env, tc.independent, n, err, tc.kind, tc.err)
			}
			if n.Kind == NestedUnbudgeted && n.Reason != tc.env[EnvUnbudgeted] {
				t.Errorf("reason %q, want %q", n.Reason, tc.env[EnvUnbudgeted])
			}
		})
	}
}

// The bound of CORESLOT_TOKEN_FDS (PROTOCOL.md sections 6 and 17): at most 512 descriptors, in
// one range or several, and a list naming more is refused before any range is expanded.
func TestTokenFDsBoundIs512(t *testing.T) {
	for _, s := range []string{"4-515", "4-259,300-555"} {
		if fds, err := ParseTokenFDs(s); err != nil || len(fds) != 512 {
			t.Errorf("ParseTokenFDs(%q) = %d fds, %v; want 512", s, len(fds), err)
		}
	}
	for _, s := range []string{"4-516", "4-259,300-556", "0-99999999999999", "0-999999999"} {
		if fds, err := ParseTokenFDs(s); err == nil {
			t.Errorf("ParseTokenFDs(%q) = %d fds, want a refusal", s, len(fds))
		}
	}
}

// A list naming one descriptor twice, directly or through overlapping ranges, does not parse.
func TestTokenFDsNamingADescriptorTwice(t *testing.T) {
	for _, s := range []string{"4,4", "4-6,5", "4-6,6-8", "4-4,4"} {
		if fds, err := ParseTokenFDs(s); err == nil {
			t.Errorf("ParseTokenFDs(%q) = %v, want a refusal", s, fds)
		}
	}
	if fds, err := ParseTokenFDs("4-6,8,7"); err != nil || !slices.Equal(fds, []int{4, 5, 6, 8, 7}) {
		t.Errorf("ParseTokenFDs(4-6,8,7) = %v, %v; want 4 5 6 8 7 in the order listed", fds, err)
	}
}

// One descriptor number: digits only, one to nine of them.
func TestParseFD(t *testing.T) {
	for s, want := range map[string]int{"3": 3, "0": 0, "007": 7, "999999999": 999999999} {
		if fd, ok := ParseFD(s); !ok || fd != want {
			t.Errorf("ParseFD(%q) = %d, %v; want %d", s, fd, ok, want)
		}
	}
	for _, s := range []string{"", "+3", "-3", " 3", "3 ", "0x3", "3.0", "1000000000"} {
		if fd, ok := ParseFD(s); ok {
			t.Errorf("ParseFD(%q) = %d, want a refusal", s, fd)
		}
	}
}
