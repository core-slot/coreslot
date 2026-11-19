//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/engine"
)

const (
	askDeadline  = 2 * time.Second // each question's
	askOutputMax = 64 << 10        // bytes of each question's output
	askWaitDelay = 200 * time.Millisecond
)

var errAskOutput = errors.New("output over 64 KiB")

// askDocker asks the docker the command would run (program, looked up on PATH as the command would
// find it) the two questions of the engagement, both at once: every context's endpoint, and the
// driver of the builder it would use. env is the command's environment; each question also gets
// CORESLOT_UNBUDGETED and CORESLOT_PROBE, so that a wrapper docker that calls coreslot run passes
// through and takes no forwarder. Standard input is /dev/null and the error output is discarded;
// each question has 2 s and 64 KiB of output, after which its process group is killed. Any failure
// is an error: the run is then not engaged.
func askDocker(ctx context.Context, program string, env []string) (docker.Answers, error) {
	path, err := exec.LookPath(program)
	if err != nil {
		return docker.Answers{}, err
	}
	env = append(slices.Clone(env), engine.EnvUnbudgeted+"=probe", engine.EnvProbe+"=1")
	var (
		wg           sync.WaitGroup
		ls, bx       []byte
		lsErr, bxErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); ls, lsErr = askOne(ctx, path, env, "context", "ls", "--format", "{{json .}}") }()
	go func() { defer wg.Done(); bx, bxErr = askOne(ctx, path, env, "buildx", "inspect") }()
	wg.Wait()
	if err := errors.Join(lsErr, bxErr); err != nil {
		return docker.Answers{}, err
	}
	var a docker.Answers
	for line := range strings.Lines(string(ls)) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var c struct{ DockerEndpoint string }
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return docker.Answers{}, fmt.Errorf("docker context ls: %w", err)
		}
		a.Endpoints = append(a.Endpoints, c.DockerEndpoint)
	}
	sc := bufio.NewScanner(bytes.NewReader(bx))
	for sc.Scan() {
		if driver, ok := strings.CutPrefix(sc.Text(), "Driver:"); ok {
			a.BuilderDriver = strings.TrimSpace(driver)
			break
		}
	}
	return a, nil
}

// askOne runs one question in a process group of its own and returns its output.
func askOne(ctx context.Context, path string, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, askDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = askWaitDelay
	out := &cappedOutput{cancel: cancel}
	cmd.Stdout = out
	err := cmd.Run()
	if out.over {
		return nil, fmt.Errorf("docker %s: %w", args[0], errAskOutput)
	}
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w", args[0], err)
	}
	return out.buf.Bytes(), nil
}

// cappedOutput keeps up to askOutputMax bytes; one more stops the question.
type cappedOutput struct {
	buf    bytes.Buffer
	over   bool
	cancel func()
}

func (c *cappedOutput) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > askOutputMax {
		c.over = true
		c.cancel()
		return 0, errAskOutput
	}
	return c.buf.Write(p)
}
