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
