//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"

	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// runCommand runs `coreslot run` on Windows, which has no budget (PROTOCOL.md section 16): the
// command runs at once with min of each resource, after one warning unless the configuration
// silences it; --strict refuses instead.
func runCommand(args []string, io IO) int {
	o, err := parseRun(args, io.Stderr)
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
	cfg := config.Config{WindowsWarning: true}
	if path, err := config.Path(); err == nil {
		if c, err := config.Load(path); err == nil {
			cfg = c
		}
	}
	if msg := windowsNotice(cfg); msg != "" {
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
		code, _ := classify(err)
		fmt.Fprintf(io.Stderr, "coreslot: cannot run %s: %v\n", argv[0], err)
		return code
	}
	_ = cmd.Wait()
	return exitStatus(cmd.ProcessState)
}

func isStoreStateError(error) bool { return false }
