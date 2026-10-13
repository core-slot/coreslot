//go:build unix && coreslot_testhooks

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pass-through starts no child that inherits its parent's grant but its command: the home
// directory's NSS fallback (getent), which its context check starts when CORESLOT_STATE_DIR is
// unset, must not get the hold's and the tokens' descriptors, or the grant's locks outlive every
// holder for as long as getent runs (up to its 5 s bound and the wait after it). Nor must the
// getent the pass-through starts for its run log once its command has exited. The password
// database is made to miss, so that the fallback runs on every host, and the getent substitute
// reports, from the environment it inherits, which of the descriptors CORESLOT_HELD_FD and
// CORESLOT_TOKEN_FDS name are open in it. The ordinary parent's getent has no context to report.
// Mutations: drop the shield main installs (the pass-through's context check starts getent with
// the grant open); expose the grant to the command without shielding it again once the command
// has started (the run log's getent gets it).
func TestPassThroughGetentInheritsNoGrant(t *testing.T) {
	home := t.TempDir()
	scratch := t.TempDir()
	report := filepath.Join(scratch, "report")
	getent := filepath.Join(scratch, "getent")
	script := `#!/bin/sh
[ "$1" = passwd ] || exit 2
{
	echo "context held=${CORESLOT_HELD_FD:-} tokens=${CORESLOT_TOKEN_FDS:-}"
	for fd in ${CORESLOT_HELD_FD:-} $(echo "${CORESLOT_TOKEN_FDS:-}" | tr ',-' '  '); do
		if [ -e /dev/fd/$fd ]; then echo "open $fd"; fi
	done
} >>"$GETENT_REPORT"
echo "me:x:$2:$2:me:$GETENT_HOME:/bin/sh"
`
	if err := os.WriteFile(getent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CORESLOT_") {
			env = append(env, kv)
		}
	}
	env = append(env, asMainEnv+"=1", "CORESLOT_TESTHOOK_PASSWD_MISS=1", "CORESLOT_TESTHOOK_GETENT="+getent,
		"GETENT_REPORT="+report, "GETENT_HOME="+home)

	r := cli(t, env, "run", "--need", "cpu=1", "--", cliBinary(t), "run", "--need", "cpu=1", "--", "true")
	if r.code != 0 {
		t.Fatalf("the nested runs exited %d: %q", r.code, r.stderr)
	}
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	nested := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "context held=3 ") {
			nested++
		}
		if strings.HasPrefix(l, "open ") {
			t.Errorf("a getent the pass-through started had the grant's descriptor %s open", strings.TrimPrefix(l, "open "))
		}
	}
	if nested < 2 {
		t.Errorf("the pass-through started getent %d times with its context, want its context check and its run log", nested)
	}
	if t.Failed() {
		t.Logf("getent's report:\n%s", b)
	}
}
