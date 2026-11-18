//go:build unix

package main

import (
	"errors"
	"strings"
	"testing"
)

func TestCommandShape(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"go", "test", "./..."}, "go test"},
		{[]string{"npm", "run", "test:unit"}, "npm run test:unit"},
		{[]string{"pytest", "-n", "8", "tests/"}, "pytest"},
		{[]string{"cargo", "build", "--release"}, "cargo build"},
		{[]string{"/usr/bin/make", "all", "check", "-j8"}, "make all check"},
		{[]string{"./gradlew", "build", "-x", "test"}, "gradlew build"},
		{[]string{"make", "CC=clang", "all"}, "make"},
		{[]string{"node", "8080"}, "node"},
	} {
		if got := commandShape(c.argv); got != c.want {
			t.Errorf("commandShape(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

// A launcher's shape is its command's, past the launcher's numbers and assignments; an option or
// an empty argument before the command keeps the launcher's name. Each case names the mutation that
// turns it red.
func TestCommandShapeSkipsLaunchers(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		// Removing a name from launchers gives that name.
		{[]string{"caffeinate", "make"}, "make"},
		{[]string{"env", "A=1", "make"}, "make"},
		{[]string{"nice", "make"}, "make"},
		{[]string{"nohup", "make"}, "make"},
		{[]string{"stdbuf", "make"}, "make"},
		{[]string{"time", "make"}, "make"},
		{[]string{"timeout", "10m", "npx", "jest", "--ci"}, "npx jest"}, // or not skipping a digit: "10m npx jest"
		// Not skipping an assignment gives "A=1"; an assignment only before a "/" gives "b=hunter2".
		{[]string{"env", "A=1", "B=2", "go", "test", "./..."}, "go test"},
		{[]string{"env", "a/b=hunter2", "deploy"}, "deploy"},
		// Not taking the command's base name gives "./scripts/x.sh".
		{[]string{"timeout", "10m", "./scripts/x.sh"}, "x.sh"},
		// Stopping after one launcher (a break after argv = argv[i:]) gives "timeout".
		{[]string{"env", "A=1", "timeout", "10m", "npm", "run", "check"}, "npm run check"},
		// Matching argv[0] rather than its base name gives "time make all".
		{[]string{"/usr/bin/time", "make", "all"}, "make all"},
		// Not stopping at an option takes it, or its value, for the command: "-n", "-k", "-i", "-S",
		// "-p".
		{[]string{"nice", "-n", "10", "make"}, "nice"},
		{[]string{"timeout", "-k", "5", "10m", "make"}, "timeout"},
		{[]string{"env", "-i", "PATH=/bin", "make"}, "env"},
		{[]string{"env", "-S", "deploy --token hunter2", "x"}, "env"},
		{[]string{"time", "-p", "make"}, "time"},
		// Skipping an option that holds "=" gives "make".
		{[]string{"timeout", "--signal=KILL", "10m", "make"}, "timeout"},
		// An argument with "=" anywhere is skipped, so no command follows here.
		{[]string{"timeout", "5", "/opt/a=b/run"}, "timeout"},
		// Not stopping at an empty argument panics.
		{[]string{"nohup", "", "make"}, "nohup"},
		// A launcher that runs no command keeps its name; not stopping there panics.
		{[]string{"env"}, "env"},
		{[]string{"env", "A=1", "timeout", "10m"}, "timeout"},
		// Unchanged, no shell string or other tool's flags read: dropping the shape's own stop at
		// "-" gives "sh -c" and "python -m pytest".
		{[]string{"sh", "-c", "go test ./..."}, "sh"},
		{[]string{"python", "-m", "pytest"}, "python"},
	} {
		if got := commandShape(c.argv); got != c.want {
			t.Errorf("commandShape(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

// Every worktree of a repository names it by the parent of the common git directory. Mutation:
// naming the checkout's own directory reddens the worktree.
func TestRepoNameWorktreesShare(t *testing.T) {
	git := func(string) (string, error) { return "/x/proj/.git", nil }
	for _, dir := range []string{"/x/proj", "/x/proj-feature"} {
		if got := repoName(dir, git); got != "proj" {
			t.Errorf("repoName(%q) = %q, want proj", dir, got)
		}
	}
	// In the main checkout git answers with a path relative to the directory it ran in.
	if got := repoName("/x/proj/sub", func(string) (string, error) { return "../.git", nil }); got != "proj" {
		t.Errorf("repoName with a relative answer = %q, want proj", got)
	}
}

// A common directory not named .git (a submodule's, under the superproject's .git/modules, or a
// bare repository's) names the repository by its own base, without a .git suffix. Mutation:
// always taking the parent names them "modules" and "x".
func TestRepoNameSubmoduleAndBare(t *testing.T) {
	for common, want := range map[string]string{"/x/super/.git/modules/lib": "lib", "/x/proj.git": "proj"} {
		if got := repoName("/y", func(string) (string, error) { return common, nil }); got != want {
			t.Errorf("repoName with the common directory %s = %q, want %s", common, got, want)
		}
	}
}

// Outside a repository, or when git fails, there is no repository name, and the label is the
// shape alone.
func TestNoRepoShapeOnly(t *testing.T) {
	if got := repoName(t.TempDir(), func(string) (string, error) { return "", errors.New("not a repository") }); got != "" {
		t.Errorf("repoName outside a repository = %q, want none", got)
	}
	if got := autoLabel([]string{"go", "test", "./..."}, t.TempDir()); got != "go test" {
		t.Errorf("autoLabel outside a repository = %q, want go test", got)
	}
}

// --label wins over the automatic label, which names an unlabelled run in the run log.
func TestExplicitLabelWins(t *testing.T) {
	dir := stateDir(t, "")
	for _, args := range [][]string{{"run", "--restart=yes", "--label", "mine", "--", "true"}, {"run", "--restart=yes", "--", "true"}} {
		if r := cli(t, baseEnv(dir), args...); r.code != 0 {
			t.Fatalf("%q: exit %d (stderr %q)", args, r.code, r.stderr)
		}
	}
	lines := runLog(t, dir)
	if len(lines) != 2 || lines[0]["label"] != "mine" {
		t.Fatalf("run log %v, want the explicit label first", lines)
	}
	if got, _ := lines[1]["label"].(string); got != "true" && !strings.HasPrefix(got, "true@") {
		t.Errorf("automatic label %q, want \"true\" or \"true@<repository>\"", got)
	}
}
