//go:build unix

package config

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGetent writes an executable shell script standing in for getent and returns its path. The
// script receives getent's arguments, `passwd <uid>`.
func fakeGetent(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "getent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The NSS fallback takes the home directory from exactly one well-formed passwd line for the uid
// asked, and refuses everything else. Mutation: taking field 5 (the GECOS field) instead of
// field 6 reddens "success"; dropping the uid comparison reddens "wrong uid"; dropping the
// one-line check reddens "two lines"; running without a deadline reddens "timeout".
func TestGetentHome(t *testing.T) {
	const uid = 4242
	missing := filepath.Join(t.TempDir(), "getent")
	for _, tc := range []struct {
		name    string
		body    string // the fake getent's script; "" means no getent at all
		want    string // the home directory, or "" when an error is expected
		wantErr string // a substring of the expected error
	}{
		{name: "success", want: "/srv/ghost",
			body: `[ "$#" = 2 ] && [ "$1" = passwd ] || exit 9
echo "ghost:x:$2:100:Ghost User:/srv/ghost:/bin/sh"`},
		{name: "absent", wantErr: "no getent at " + missing},
		{name: "no entry", body: "exit 2", wantErr: "has no entry for uid 4242"},
		{name: "fails", body: "exit 1", wantErr: "exit status 1"},
		{name: "garbage", body: "echo garbage", wantErr: "1 colon-separated fields, not 7"},
		{name: "empty", body: ":", wantErr: "not exactly one line"},
		{name: "six fields", body: `echo "ghost:x:$2:100::/srv/ghost"`, wantErr: "6 colon-separated fields, not 7"},
		{name: "eight fields", body: `echo "ghost:x:$2:100::/srv/ghost:/bin/sh:x"`, wantErr: "8 colon-separated fields, not 7"},
		{name: "two lines", body: `echo "ghost:x:$2:100::/srv/ghost:/bin/sh"; echo "ghost:x:$2:100::/srv/ghost:/bin/sh"`,
			wantErr: "not exactly one line"},
		{name: "no newline", body: `printf "ghost:x:$2:100::/srv/ghost:/bin/sh"`, wantErr: "not exactly one line"},
		{name: "wrong uid", body: `echo "root:x:0:0:root:/root:/bin/sh"`, wantErr: `the entry of uid "0", not 4242`},
		{name: "relative home", body: `echo "ghost:x:$2:100::home/ghost:/bin/sh"`, wantErr: "not an absolute path"},
		{name: "empty home", body: `echo "ghost:x:$2:100:::/bin/sh"`, wantErr: "not an absolute path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := missing
			if tc.body != "" {
				path = fakeGetent(t, tc.body)
			}
			home, err := getentHome([]string{path}, uid, 5*time.Second)
			if tc.want != "" {
				if err != nil || home != tc.want {
					t.Fatalf("getentHome() = %q, %v; want %q", home, err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("getentHome() = %q, %v; want an error containing %q", home, err, tc.wantErr)
			}
		})
	}
}

// A getent that does not answer is abandoned at the deadline, even when a process it started
// keeps its standard output open (the shell's sleep below holds the pipe after the shell is
// killed). Mutation: no deadline, or no WaitDelay, holds the call for the full 30 s sleep.
func TestGetentHomeTimeout(t *testing.T) {
	path := fakeGetent(t, `sleep 30; echo "ghost:x:$2:100::/srv/ghost:/bin/sh"`)
	start := time.Now()
	home, err := getentHome([]string{path}, 4242, 300*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "did not answer within 300ms") {
		t.Fatalf("getentHome() = %q, %v; want a timeout", home, err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("getentHome() returned after %v; want about 300ms plus the wait delay", elapsed)
	}
}

// The candidates are tried in order and the first that exists is used; a later one is never run
// once an earlier one exists, even when the earlier one fails.
func TestGetentHomeCandidates(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "getent")
	good := fakeGetent(t, `echo "ghost:x:$2:100::/srv/ghost:/bin/sh"`)
	if home, err := getentHome([]string{missing, good}, 4242, 5*time.Second); err != nil || home != "/srv/ghost" {
		t.Errorf("getentHome(missing, good) = %q, %v; want /srv/ghost", home, err)
	}
	bad := fakeGetent(t, "exit 2")
	if home, err := getentHome([]string{bad, good}, 4242, 5*time.Second); err == nil {
		t.Errorf("getentHome(bad, good) = %q; want the first candidate's failure", home)
	}
	dir := t.TempDir() // a directory is not a getent
	if home, err := getentHome([]string{dir, good}, 4242, 5*time.Second); err != nil || home != "/srv/ghost" {
		t.Errorf("getentHome(directory, good) = %q, %v; want /srv/ghost", home, err)
	}
}

// homeOf falls back to getent only when the lookup has no record for the uid, never starts it
// when the lookup succeeds, and on a failed fallback returns the lookup's own error, wrapped.
// Mutation: falling back on every error reddens "other error"; falling back before the lookup
// reddens "lookup succeeds"; returning the fallback's error alone reddens "fallback fails".
func TestHomeOfFallback(t *testing.T) {
	const uid = 4242
	unknown := func(string) (*user.User, error) { return nil, user.UnknownUserIdError(uid) }
	// The marker getent records that it ran, then answers for the uid.
	markerGetent := func(t *testing.T) (path, marker string) {
		marker = filepath.Join(t.TempDir(), "ran")
		path = fakeGetent(t, `: > '`+marker+`'
echo "ghost:x:$2:100::/srv/ghost:/bin/sh"`)
		return path, marker
	}
	ran := func(marker string) bool {
		_, err := os.Stat(marker)
		return err == nil
	}

	t.Run("lookup succeeds", func(t *testing.T) {
		path, marker := markerGetent(t)
		found := func(id string) (*user.User, error) { return &user.User{Uid: id, HomeDir: "/srv/real"}, nil }
		home, err := homeOf(uid, found, []string{path}, 5*time.Second)
		if err != nil || home != "/srv/real" {
			t.Fatalf("homeOf() = %q, %v; want /srv/real", home, err)
		}
		if ran(marker) {
			t.Error("getent ran although the lookup succeeded")
		}
	})
	t.Run("unknown uid", func(t *testing.T) {
		path, marker := markerGetent(t)
		home, err := homeOf(uid, unknown, []string{path}, 5*time.Second)
		if err != nil || home != "/srv/ghost" {
			t.Fatalf("homeOf() = %q, %v; want /srv/ghost from getent", home, err)
		}
		if !ran(marker) {
			t.Error("the home directory did not come from getent")
		}
	})
	t.Run("no passwd file", func(t *testing.T) {
		path, _ := markerGetent(t)
		noFile := func(string) (*user.User, error) {
			return nil, &fs.PathError{Op: "open", Path: "/etc/passwd", Err: fs.ErrNotExist}
		}
		if home, err := homeOf(uid, noFile, []string{path}, 5*time.Second); err != nil || home != "/srv/ghost" {
			t.Fatalf("homeOf() = %q, %v; want /srv/ghost from getent", home, err)
		}
	})
	t.Run("other error", func(t *testing.T) {
		path, marker := markerGetent(t)
		broken := errors.New("user: lookup userid 4242: input/output error")
		failing := func(string) (*user.User, error) { return nil, broken }
		home, err := homeOf(uid, failing, []string{path}, 5*time.Second)
		if !errors.Is(err, broken) {
			t.Fatalf("homeOf() = %q, %v; want the lookup's error", home, err)
		}
		if ran(marker) {
			t.Error("getent ran for an error that is not a missing record")
		}
	})
	t.Run("fallback fails", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "getent")
		home, err := homeOf(uid, unknown, []string{missing}, 5*time.Second)
		var unknownErr user.UnknownUserIdError
		if !errors.As(err, &unknownErr) {
			t.Fatalf("homeOf() = %q, %v; want the lookup's UnknownUserIdError", home, err)
		}
		for _, want := range []string{"unknown userid " + strconv.Itoa(uid), "NSS fallback", "no getent at " + missing} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("homeOf() error %q does not mention %q", err, want)
			}
		}
	})
	t.Run("empty home", func(t *testing.T) {
		empty := func(id string) (*user.User, error) { return &user.User{Uid: id}, nil }
		if home, err := homeOf(uid, empty, nil, 5*time.Second); err == nil {
			t.Fatalf("homeOf() = %q; want an error for an empty home directory", home)
		}
	})
}
