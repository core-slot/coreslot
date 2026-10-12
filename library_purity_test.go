package coreslot

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestLibraryPurity parses every non-test Go file outside cmd/, except the files of a command
// (package main, such as the conformance suite's helper), and fails on output, process exit and
// signal handling: the library never prints (see violations), never chooses an exit
// code (os.Exit) and never installs signal handlers (signal.Notify, signal.Ignore).
func TestLibraryPurity(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "cmd", "testdata", ".git", "evidence", "docs":
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

// violations finds, in f: fmt.Print*; fmt.Fprint* whose writer is os.Stdout or os.Stderr; any
// use of log or log/slog (whatever their local names); os.Exit; os.Stdout and os.Stderr
// Write* calls; signal.Notify and signal.Ignore; and the builtins print and println.
func violations(f *ast.File) []violation {
	names := imported(f)
	isStd := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		return ok && names[x.Name] == "os" && (sel.Sel.Name == "Stdout" || sel.Sel.Name == "Stderr")
	}
	var out []violation
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			switch fun := n.Fun.(type) {
			case *ast.Ident:
				// A builtin is unresolved by the parser; a local func of that name is not.
				if (fun.Name == "print" || fun.Name == "println") && fun.Obj == nil {
					out = append(out, violation{fun.Pos(), "builtin " + fun.Name})
				}
			case *ast.SelectorExpr:
				if x, ok := fun.X.(*ast.Ident); ok && names[x.Name] == "fmt" &&
					strings.HasPrefix(fun.Sel.Name, "Fprint") && len(n.Args) > 0 && isStd(n.Args[0]) {
					out = append(out, violation{fun.Pos(), "fmt." + fun.Sel.Name + " to os.Stdout/os.Stderr"})
				}
				if isStd(fun.X) && strings.HasPrefix(fun.Sel.Name, "Write") {
					out = append(out, violation{fun.Pos(), "os.Stdout/os.Stderr." + fun.Sel.Name})
				}
			}
		case *ast.SelectorExpr:
			x, ok := n.X.(*ast.Ident)
			if !ok {
				return true
			}
			what := x.Name + "." + n.Sel.Name
			switch names[x.Name] {
			case "fmt":
				if strings.HasPrefix(n.Sel.Name, "Print") {
					out = append(out, violation{n.Pos(), what})
				}
			case "log", "log/slog":
				out = append(out, violation{n.Pos(), what})
			case "os":
				if n.Sel.Name == "Exit" {
					out = append(out, violation{n.Pos(), what})
				}
			case "os/signal":
				if n.Sel.Name == "Notify" || n.Sel.Name == "Ignore" {
					out = append(out, violation{n.Pos(), what})
				}
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
)

func bad(w io.Writer) {
	fmt.Println("a")
	fmt.Fprintln(os.Stderr, "b")
	fmt.Fprintf(os.Stdout, "c")
	os.Stdout.Write(nil)
	os.Stderr.WriteString("d")
	lg.Print("e")
	sl.Info("f")
	os.Exit(1)
	sig.Notify(nil)
	sig.Ignore()
	print("g")
	println("h")
}

func good(w io.Writer) error {
	_, err := fmt.Fprintf(w, "ok")
	_ = fmt.Sprintf("ok")
	_ = fmt.Errorf("ok")
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
	want := []string{"fmt.Println", "fmt.Fprintln to os.Stdout/os.Stderr", "fmt.Fprintf to os.Stdout/os.Stderr",
		"os.Stdout/os.Stderr.Write", "os.Stdout/os.Stderr.WriteString", "lg.Print", "sl.Info", "os.Exit",
		"sig.Notify", "sig.Ignore", "builtin print", "builtin println"}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("not detected: %s (got %v)", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d findings %v, want exactly %d (the good function has none)", len(got), got, len(want))
	}
}
