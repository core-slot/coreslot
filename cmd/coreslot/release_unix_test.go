//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A run whose state directory is removed and recreated while its command runs finds out when it
// releases (I1): it prints the state error, writes nothing into the new directory (no hold
// removal, no section entry, so no admit.lock, FORMAT or gen, and no run-log line), and exits
// with the command's own status. A fail-open run's unbudgeted hold follows the same rule.
func TestRunReleaseAfterTheStateDirectoryWasReplaced(t *testing.T) {
	const recreate = `rm -rf "$S" && mkdir "$S"`
	const withFormat = recreate + ` && printf 'coreslot-protocol 1\n' > "$S/FORMAT"`
	for _, c := range []struct{ name, cfg, script string }{
		{"admitted", "", recreate},
		{"admitted, new FORMAT", "", withFormat},
		{"fail-open", "[resources]\ncpu = 0\n", recreate},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := stateDir(t, c.cfg)
			env := append(baseEnv(dir), "S="+dir)
			r := cli(t, env, "run", "--need", "cpu=1", "--", "sh", "-c", c.script+" && exit 7")
			if r.code != 7 {
				t.Fatalf("exit %d, want the command's 7; stderr:\n%s", r.code, r.stderr)
			}
			if !strings.Contains(r.stderr, "state error at release") {
				t.Errorf("stderr does not report the release's state error:\n%s", r.stderr)
			}
			names, err := os.ReadDir(dir)
			var got []string
			for _, n := range names {
				got = append(got, n.Name())
			}
			want := ""
			if c.script == withFormat {
				want = "FORMAT"
			}
			if err != nil || strings.Join(got, " ") != want {
				t.Errorf("the new state directory holds %q, %v; want %q, what the command left", got, err, want)
			}
			if _, err := os.Lstat(filepath.Join(dir, "log")); err == nil {
				t.Error("a run log was written into the new state directory")
			}
		})
	}
}
