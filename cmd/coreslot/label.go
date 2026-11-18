package main

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/core-slot/coreslot/internal/spec"
)

// maxShapeWords is how many words the command's shape has at most, the executable included.
const maxShapeWords = 3

// launchers run the command given after their own arguments, so a shape is taken from that command.
var launchers = map[string]bool{
	"caffeinate": true, "env": true, "nice": true, "nohup": true, "stdbuf": true, "time": true, "timeout": true,
}

// wrapped is the command a launcher in argv[0] runs: argv from the first argument after it that
// neither starts with a digit nor contains "=", taken again while that command is itself a
// launcher. It is argv, the launcher's own name kept, when an empty argument or one that starts
// with "-" comes first, since an option's value cannot be told from a command, or when no command
// follows.
func wrapped(argv []string) []string {
	for launchers[filepath.Base(argv[0])] {
		i := 1
		for i < len(argv) && argv[i] != "" && argv[i][0] != '-' &&
			(argv[i][0] >= '0' && argv[i][0] <= '9' || strings.Contains(argv[i], "=")) {
			i++
		}
		if i == len(argv) || argv[i] == "" || argv[i][0] == '-' {
			break
		}
		argv = argv[i:]
	}
	return argv
}

// commandShape is the base name of the executable, or of the command a launcher runs (see
// wrapped), and its leading word-like arguments, at most maxShapeWords in all: it stops at the
// first argument that starts with "-" or a digit, or contains "/", "=" or ".".
func commandShape(argv []string) string {
	argv = wrapped(argv)
	words := []string{filepath.Base(argv[0])}
	for _, a := range argv[1:] {
		if len(words) == maxShapeWords || a == "" || a[0] == '-' || a[0] >= '0' && a[0] <= '9' ||
			strings.ContainsAny(a, "/=.") {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// gitCommonDir is `git rev-parse --git-common-dir` run in dir.
func gitCommonDir(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// repoName is the name of the repository dir belongs to, from the git common directory, which every
// worktree of a repository shares: the base of its parent when it is named .git, else its own base
// less a .git suffix. It is "" outside a repository and when git fails.
func repoName(dir string, git func(dir string) (string, error)) string {
	common, err := git(dir)
	if err != nil || common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) == ".git" {
		return filepath.Base(filepath.Dir(common))
	}
	return strings.TrimSuffix(filepath.Base(common), ".git") // a submodule's or a bare repository's
}

// autoLabel is the label of a run that names none: the command's shape, then "@" and the name of
// the repository dir belongs to. It is "" when that is not a valid label.
func autoLabel(argv []string, dir string) string {
	label := commandShape(argv)
	if repo := repoName(dir, gitCommonDir); repo != "" {
		label += "@" + repo
	}
	if spec.ValidateLabel(label) != nil {
		return ""
	}
	return label
}
