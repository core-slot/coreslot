//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

// standIn writes an executable docker whose body is script into a directory of its own.
func standIn(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Both questions are answered in the shapes the CLI prints: every context's DockerEndpoint, in
// order, and the current builder's driver.
func TestAskReadsTheCLI(t *testing.T) {
	endpoints := []string{"unix:///var/run/docker.sock", "tcp://192.0.2.10:2376", "unix:///Users/tester/.docker/run/docker.sock"}
	dir := dockertest.CLI(t, endpoints, "docker-container")
	got, err := askDocker(context.Background(), filepath.Join(dir, "docker"), os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if want := (docker.Answers{Endpoints: endpoints, BuilderDriver: "docker-container"}); !reflect.DeepEqual(got, want) {
		t.Errorf("askDocker = %+v, want %+v", got, want)
	}
}

// A docker that hangs and leaves a grandchild is killed with its whole process group at the 2 s
// deadline, and both questions run at once: two hangs return within 2.5 s. Mutations: kill only
// the child (no process group) → the grandchild survives, red; ask one question after the other →
// over 4 s, red.
func TestAskKillsGroupAtDeadline(t *testing.T) {
	dir := t.TempDir()
	path := standIn(t, `sleep 10 &
echo $! > "`+dir+`/grandchild.$1"
sleep 10
`)
	start := time.Now()
	_, err := askDocker(context.Background(), path, os.Environ())
	if took := time.Since(start); err == nil || took > 2500*time.Millisecond {
		t.Fatalf("askDocker = %v after %v; want an error within 2.5 s", err, took)
	}
	for _, q := range []string{"context", "buildx"} {
		b, err := os.ReadFile(filepath.Join(dir, "grandchild."+q))
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		gone := false
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
				gone = true
				break
			}
		}
		if !gone {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("the %s question's grandchild %d outlived the deadline", q, pid)
		}
	}
}

// Output over 64 KiB is an error, even when every line of it is a well-formed context, and the
// docker printing it is stopped. Mutation: read without a cap → no error, red.
func TestAskOutputCap(t *testing.T) {
	path := standIn(t, `if [ "$1" = buildx ]; then echo "Driver: docker"; exit 0; fi
yes '{"DockerEndpoint":"unix:///var/run/docker.sock"}' | head -n 2100
`)
	if _, err := askDocker(context.Background(), path, os.Environ()); err == nil {
		t.Fatal("askDocker read 100 KiB of output without an error")
	}
}

// Each question's environment is the command's plus CORESLOT_UNBUDGETED and CORESLOT_PROBE, so a
// wrapper docker that calls coreslot run passes through and takes no forwarder. Mutation: leave
// out CORESLOT_PROBE → red.
func TestAskEnvCarriesProbe(t *testing.T) {
	dir := t.TempDir()
	path := standIn(t, `env > "`+dir+`/env.$1"
if [ "$1" = buildx ]; then echo "Driver: docker"; fi
`)
	if _, err := askDocker(context.Background(), path, append(os.Environ(), "MARK=kept")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"context", "buildx"} {
		b, err := os.ReadFile(filepath.Join(dir, "env."+q))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for _, want := range []string{"CORESLOT_UNBUDGETED=probe", "CORESLOT_PROBE=1", "MARK=kept"} {
			found := false
			for _, l := range lines {
				found = found || l == want
			}
			if !found {
				t.Errorf("the %s question's environment lacks %s", q, want)
			}
		}
	}
}

// No docker on PATH is an error, not an answer. Mutation: return empty answers → no error, red.
func TestAskNoDockerOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if a, err := askDocker(context.Background(), "docker", os.Environ()); err == nil {
		t.Fatalf("askDocker with no docker on PATH = %+v, nil", a)
	}
}

// A question that fails is an error, even when it printed something. Mutation: ignore the exit
// status → no error, red.
func TestAskFailedQuestion(t *testing.T) {
	path := standIn(t, `if [ "$1" = context ]; then echo '{"DockerEndpoint":"unix:///x"}'; else echo "Driver: docker"; fi
exit 1
`)
	if _, err := askDocker(context.Background(), path, os.Environ()); err == nil {
		t.Fatal("askDocker took the answers of a docker that exited 1")
	}
}
