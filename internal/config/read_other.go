//go:build !unix

package config

import (
	"fmt"
	"io"
	"os"
)

// readRegular reads the configuration file at path through symbolic links. A path that does not
// exist, a dangling symlink included, returns an error matching fs.ErrNotExist. The target is
// examined with stat before it is opened, so a non-regular file is refused rather than waited on.
func readRegular(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, notRegularError{fmt.Sprintf("not a regular file (%v)", fi.Mode().Type())}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
