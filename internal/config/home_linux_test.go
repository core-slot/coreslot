package config

import (
	"os"
	"os/user"
	"strconv"
	"testing"
)

// The system's getent, found at the fixed locations, parses to the home directory the password
// database gives this process's uid: the strict parse accepts real glibc output, not only the
// fake scripts' lines.
func TestGetentHomeMatchesSystem(t *testing.T) {
	uid := os.Getuid()
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		t.Skipf("uid %d has no passwd entry to compare with: %v", uid, err)
	}
	found := false
	for _, p := range getentPaths {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			found = true
		}
	}
	if !found {
		t.Skipf("no getent at %v", getentPaths)
	}
	home, err := getentHome(getentPaths, uid, getentTimeout)
	if err != nil || home != u.HomeDir {
		t.Fatalf("getentHome(uid %d) = %q, %v; want %q", uid, home, err, u.HomeDir)
	}
}
