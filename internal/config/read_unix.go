//go:build unix

package config

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// readRegular reads the configuration file at path through symbolic links. A path that does not
// exist, a dangling symlink included, returns an error matching fs.ErrNotExist. The file is
// opened with O_NONBLOCK and examined with fstat before any read, so a FIFO (whose blocking open
// would wait for a writer) or a device is refused at once rather than waited on (PROTOCOL.md
// section 5.3).
func readRegular(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, notRegularError{fmt.Sprintf("not a regular file (%v)", fi.Mode().Type())}
	}
	return io.ReadAll(f)
}
