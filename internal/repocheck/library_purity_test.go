package repocheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestLibraryPurity parses every non-test Go file outside the programs (cmd/ and bench/), except
// the files of a command (package main, such as the conformance suite's helper), and fails on
// what only a program may do (see violations): the library never prints or touches the standard
// streams, never chooses an exit code, never handles or sends signals, and never changes
// process-wide state its caller owns (the environment, the working directory, the umask, the
// resource limits, GOMAXPROCS).
func TestLibraryPurity(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == "cmd" || path == "bench" { // the programs at the top level, not any such name
				return filepath.SkipDir
			}
			switch d.Name() {
			case "testdata", ".git", "evidence", "docs":
				if path != "." {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		if f.Name.Name == "main" {
			return nil // a command, not the library
		}
		checked++
		for _, v := range violations(f) {
			t.Errorf("%s: %s in library code", fset.Position(v.pos), v.what)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no file checked")
	}
}

// imported maps each local package name of f to its import path.
func imported(f *ast.File) map[string]string {
	names := map[string]string{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names[name] = p
	}
	return names
}

// processWide maps an import path to the functions of it that change process-wide state, end
// the process or signal a process.
var processWide = map[string][]string{
	"os":                    {"Exit", "Setenv", "Unsetenv", "Clearenv", "Chdir"},
	"syscall":               {"Exit", "Setenv", "Unsetenv", "Clearenv", "Chdir", "Fchdir", "Umask", "Setrlimit", "Kill"},
	"golang.org/x/sys/unix": {"Exit", "Setenv", "Unsetenv", "Clearenv", "Chdir", "Fchdir", "Umask", "Setrlimit", "Prlimit", "Kill"},
	"runtime":               {"GOMAXPROCS"},
}

// violations finds, in f: any reference to os.Stdin, os.Stdout or os.Stderr, called or used as a
// value; fmt.Print*; any use of log, log/slog or os/signal (whatever their local names); the
// functions of processWide; a method Kill called without arguments on a value (os.Process.Kill:
// without type information every such call counts); and the builtins print and println.
func violations(f *ast.File) []violation {
	names := imported(f)
	var out []violation
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			// A builtin is unresolved by the parser; a local func of that name is not.
			if fun, ok := n.Fun.(*ast.Ident); ok && (fun.Name == "print" || fun.Name == "println") && fun.Obj == nil {
				out = append(out, violation{fun.Pos(), "builtin " + fun.Name})
			}
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Kill" && len(n.Args) == 0 {
				if x, ok := sel.X.(*ast.Ident); !ok || names[x.Name] == "" { // a value, not a package
					out = append(out, violation{sel.Pos(), types.ExprString(sel)})
				}
			}
		case *ast.SelectorExpr:
			x, ok := n.X.(*ast.Ident)
			if !ok {
				return true
			}
			what := x.Name + "." + n.Sel.Name
			switch path := names[x.Name]; {
			case path == "fmt":
				if strings.HasPrefix(n.Sel.Name, "Print") {
					out = append(out, violation{n.Pos(), what})
				}
			case path == "log", path == "log/slog", path == "os/signal":
				out = append(out, violation{n.Pos(), what})
			case path == "os" && (n.Sel.Name == "Stdin" || n.Sel.Name == "Stdout" || n.Sel.Name == "Stderr"):
				out = append(out, violation{n.Pos(), what})
			case slices.Contains(processWide[path], n.Sel.Name):
				out = append(out, violation{n.Pos(), what})
			}
		}
		return true
	})
	return out
}

type violation struct {
	pos  token.Pos
	what string
}

// TestLibraryPurityDetector feeds the detector one sample of each forbidden pattern, and the
// allowed look-alikes, so that a detector that misses one is red.
func TestLibraryPurityDetector(t *testing.T) {
	const src = `package x

import (
	"fmt"
	"io"
	lg "log"
	sl "log/slog"
	"os"
	sig "os/signal"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

func bad(w io.Writer) {
	fmt.Println("a")
	fmt.Fprintln(os.Stderr, "b")
	fmt.Fprintf(os.Stdout, "c")
	os.Stdout.Write(nil)
	io.WriteString(os.Stderr, "d")
	e := os.Stderr
	fmt.Fprintln(e, "e")
	_ = os.Stdin
	lg.Print("f")
	sl.Info("g")
	os.Exit(1)
	syscall.Exit(1)
	unix.Exit(1)
	sig.Notify(nil)
	sig.Ignore()
	sig.Reset()
	sig.Stop(nil)
	os.Setenv("k", "v")
	os.Unsetenv("k")
	os.Clearenv()
	os.Chdir("/")
	syscall.Umask(0)
	unix.Umask(0)
	runtime.GOMAXPROCS(1)
	print("h")
	println("i")
	syscall.Setrlimit(syscall.RLIMIT_NOFILE, nil)
	unix.Setrlimit(unix.RLIMIT_NOFILE, nil)
	unix.Prlimit(0, unix.RLIMIT_NOFILE, nil, nil)
	syscall.Kill(1, syscall.SIGTERM)
	unix.Kill(1, unix.SIGTERM)
	p, _ := os.FindProcess(1)
	p.Kill()
	cmd.Process.Kill()
	start().Process.Kill()
}

func good(w io.Writer) error {
	_, err := fmt.Fprintf(w, "ok")
	_ = fmt.Sprintf("ok")
	_ = fmt.Errorf("ok")
	_ = os.Getenv("k")
	_ = runtime.NumGoroutine()
	_ = unix.Getpid()
	var lim unix.Rlimit
	_ = unix.Getrlimit(unix.RLIMIT_NOFILE, &lim)
	return err
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range violations(f) {
		got = append(got, v.what)
	}
	want := []string{"fmt.Println", "os.Stderr", "os.Stdout", "os.Stdout", "os.Stderr", "os.Stderr", "os.Stdin",
		"lg.Print", "sl.Info", "os.Exit", "syscall.Exit", "unix.Exit",
		"sig.Notify", "sig.Ignore", "sig.Reset", "sig.Stop",
		"os.Setenv", "os.Unsetenv", "os.Clearenv", "os.Chdir", "syscall.Umask", "unix.Umask",
		"runtime.GOMAXPROCS", "builtin print", "builtin println",
		"syscall.Setrlimit", "unix.Setrlimit", "unix.Prlimit", "syscall.Kill", "unix.Kill", "p.Kill", "cmd.Process.Kill", "start().Process.Kill"}
	if !slices.Equal(got, want) {
		t.Errorf("findings:\n got %v\nwant %v", got, want)
	}
}
