//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// runCommand runs `coreslot run` on Windows, which has no budget (PROTOCOL.md section 16): the
// command runs at once with min of each resource, after one warning unless the configuration
// silences it; --strict refuses instead.
func runCommand(args []string, io IO) int {
	o, err := parseRun(args, io)
	if errors.Is(err, errHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: %v\n", err)
		return exitUsage
	}
	if o.strict {
		fmt.Fprintln(io.Stderr, "coreslot: Windows has no coreslot budget; refusing (--strict)")
		return o.refusalCode
	}
	if msg := windowsNotice(windowsConfig()); msg != "" {
		fmt.Fprintln(io.Stderr, msg)
	}
	argv := substitute(o.argv, o.minGrant(), o.subst)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = io.Stdin, io.Stdout, io.Stderr
	cmd.Env = os.Environ()
	for name, n := range o.minGrant() {
		cmd.Env = append(cmd.Env, fmt.Sprintf("CORESLOT_GRANTED_%s=%d", spec.EnvName(name), n))
	}
	// The console delivers Ctrl-C to the command too; the run only waits for it.
	signal.Notify(make(chan os.Signal, 1), os.Interrupt)
	if err := cmd.Start(); err != nil {
		code, _ := classify(&execError{Err: err})
		fmt.Fprintf(io.Stderr, "coreslot: cannot run %s: %v\n", argv[0], err)
		return code
	}
	_ = cmd.Wait()
	return exitStatus(cmd.ProcessState)
}

// engineStateReason: Windows has no budget and no engine, so no error is the engine's state error.
func engineStateReason(error) (string, bool) { return "", false }
