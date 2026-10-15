// Package repocheck holds the repository's own checks: the pinned exported API, the library's
// purity, PROTOCOL.md's contract inventory and the hygiene of the repository's files. It has no
// code of its own, only tests, and they read the repository rather than import it.
package repocheck

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain runs every test from the repository root, the nearest directory above the package
// that holds a go.mod, so that the checks name the repository's paths as they are written.
func TestMain(m *testing.M) {
	root, err := moduleRoot()
	if err == nil {
		err = os.Chdir(root)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "repocheck:", err)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// moduleRoot walks up from the working directory to the first directory that holds a go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the working directory")
		}
		dir = parent
	}
}
