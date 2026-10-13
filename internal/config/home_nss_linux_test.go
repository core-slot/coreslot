//go:build linux && coreslot_testhooks

package config

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestHomeNSSFallbackEndToEnd runs Home, StateDir and Locate as a uid that /etc/passwd does not
// list, as on a host whose accounts come from LDAP, SSSD, systemd-homed or winbind. It needs such
// a uid, so it skips for any account with a passwd entry; run it as one without, for example in
// the golang image as root:
//
//	go test -c -tags coreslot_testhooks -o /tmp/config.test ./internal/config
//	setpriv --reuid=4242 --regid=4242 --clear-groups /tmp/config.test -test.run NSSFallback -test.v
//
// No NSS source of that image (files, compat, dns, hesiod) can serve an account that /etc/passwd
// lacks, so the answer comes from a getent substitute named by CORESLOT_TESTHOOK_GETENT; the
// system's getent is run first and must report the uid unknown.
func TestHomeNSSFallbackEndToEnd(t *testing.T) {
	uid := os.Getuid()
	var unknown user.UnknownUserIdError
	if _, err := user.LookupId(strconv.Itoa(uid)); !errors.As(err, &unknown) {
		t.Skipf("uid %d is in the password database (lookup error %v); run this test as a uid without a passwd entry", uid, err)
	}
	t.Setenv(StateDirEnv, "")

	// The system's getent knows no more than /etc/passwd here: the lookup's own error comes back,
	// saying that the fallback was tried.
	t.Setenv("CORESLOT_TESTHOOK_GETENT", "")
	if home, err := Home(); !errors.As(err, &unknown) || !strings.Contains(err.Error(), "NSS fallback") {
		t.Fatalf("Home() with the system's getent = %q, %v; want the lookup's error naming the NSS fallback", home, err)
	} else {
		t.Logf("Home() with the system's getent: %v", err)
	}

	// An NSS source that knows the uid: the home directory, the state directory and the
	// configuration all follow it.
	dir := t.TempDir()
	getent := filepath.Join(dir, "getent")
	script := "#!/bin/sh\n[ \"$1\" = passwd ] && [ \"$2\" = " + strconv.Itoa(uid) + " ] || exit 2\n" +
		"echo \"ghost:x:$2:$2:NSS user:/nss/ghost:/bin/sh\"\n"
	if err := os.WriteFile(getent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CORESLOT_TESTHOOK_GETENT", getent)
	home, err := Home()
	if err != nil || home != "/nss/ghost" {
		t.Fatalf("Home() = %q, %v; want /nss/ghost", home, err)
	}
	wantDir := filepath.Join("/nss/ghost", ".local", "state", "coreslot")
	wantCfg := filepath.Join("/nss/ghost", ".config", "coreslot", "config.toml")
	if d, p, err := Locate(); err != nil || d != wantDir || p != wantCfg {
		t.Fatalf("Locate() = %q, %q, %v; want %q, %q", d, p, err, wantDir, wantCfg)
	}
}
