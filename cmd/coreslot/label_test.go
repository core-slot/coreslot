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

// A launcher's shape is its command's, past the launcher's own arguments. Each case names the
// mutation of wrapped that turns it red.
func TestCommandShapeSkipsLaunchers(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		// Not skipping an argument that starts with a digit gives "10m".
		{[]string{"timeout", "10m", "npx", "jest", "--ci"}, "npx jest"},
		// Not skipping an argument that starts with "-" gives "-n".
		{[]string{"nice", "-n", "10", "cargo", "build"}, "cargo build"},
		// Not skipping an assignment gives "A=1".
		{[]string{"env", "A=1", "B=x/y", "go", "test", "./..."}, "go test"},
		// Taking any "=" for an assignment gives "all"; skipping a path as well gives "timeout".
		{[]string{"timeout", "5", "/opt/a=b/run", "all"}, "run all"},
		{[]string{"timeout", "10m", "./scripts/x.sh"}, "x.sh"},
		// Skipping one launcher only, not every one in a row, gives "timeout".
		{[]string{"env", "A=1", "timeout", "10m", "npm", "run", "check"}, "npm run check"},
		// Matching the path rather than its base name gives "time".
		{[]string{"/usr/bin/time", "-p", "make", "all"}, "make all"},
		// Reading an empty argument's first byte panics.
		{[]string{"nohup", "", "make"}, "make"},
		// A launcher that runs no command keeps its name; not stopping there panics.
		{[]string{"env"}, "env"},
		{[]string{"env", "A=1"}, "env"},
		{[]string{"env", "A=1", "timeout", "10m"}, "timeout"},
		// The documented limit: an option's bare-word value is taken for the command.
		{[]string{"timeout", "-s", "KILL", "10m", "make"}, "KILL"},
		// Unchanged: a shell string and another tool's flags are not read, and a launcher's name
		// past argv[0] is an ordinary word.
		{[]string{"sh", "-c", "go test ./..."}, "sh"},
		{[]string{"python", "-m", "pytest"}, "python"},
		{[]string{"make", "env", "check"}, "make env check"},
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
