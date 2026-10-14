// Command coreslot runs commands inside a cooperative, machine-wide budget of CPU cores and
// other declared resources, shared through the flock(2) protocol of PROTOCOL.md.
//
//	coreslot run [--need RES=MIN..MAX]... [flags] -- COMMAND...
//	coreslot status | cancel | doctor | version
//
// All printing, signal handling and exit statuses live here; the library does none of them.
package main

import (
	"fmt"
	"os"

	"github.com/core-slot/coreslot/internal/config"
)

// version is the CLI's version; release builds set it with -ldflags "-X main.version=...".
// Without that, cliVersion falls back to the build information (version.go).
var version = devVersion

const devVersion = "0.0.0-dev"

// IO is the standard streams of one invocation. The command a run starts inherits them as they
// are, so Stdin is also what terminal mode is decided from.
type IO struct {
	Stdin, Stdout, Stderr *os.File
}

const usage = `usage: coreslot <command> [arguments]

commands:
  run      run a command inside the budget
  status   show the budget, its holders and its queue
  cancel   cancel a waiting request
  doctor   check the state directory and the configuration
  version  print the version and the protocol formats implemented`

func main() {
	shieldInherited()
	installLockHooks()
	os.Exit(dispatch(os.Args[1:], IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
}

// dispatch runs one subcommand and returns the process's exit status.
func dispatch(args []string, io IO) int {
	if len(args) == 0 {
		fmt.Fprintln(io.Stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "run":
		return runCommand(args[1:], io)
	case "version", "--version":
		return versionCommand(args[1:], io)
	case "status":
		return statusCommand(args[1:], io)
	case "cancel":
		return cancelCommand(args[1:], io)
	case "doctor":
		return doctorCommand(args[1:], io)
	case "help", "-h", "--help":
		fmt.Fprintln(io.Stdout, usage)
		return 0
	}
	fmt.Fprintf(io.Stderr, "coreslot: unknown command %q\n%s\n", args[0], usage)
	return exitUsage
}

// windowsNotice is the warning a Windows run prints before running its command without a
// budget (PROTOCOL.md section 16), or "" when [platform] windows_warning = false silences it.
func windowsNotice(cfg config.Config) string {
	if !cfg.WindowsWarning {
		return ""
	}
	return "coreslot: Windows has no coreslot budget; running the command without one " +
		"(set windows_warning = false under [platform] to silence this)"
}
