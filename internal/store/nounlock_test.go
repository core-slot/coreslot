//go:build unix

package store

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot walks up from the package directory to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}

// flockPackages are the import paths whose Flock takes an operation argument.
var flockPackages = map[string]bool{"golang.org/x/sys/unix": true, "syscall": true}

// TestNoUnlockInPackage parses every Go file of every package in the module and fails on any
// flock(2) call whose operation is not built from LOCK_EX and LOCK_NB alone: LOCK_UN, the
// literal 8, LOCK_SH, a variable or any other expression. Release is by closing (PROTOCOL.md
// section 6). It also refuses Flock used as a value, a dot-import of unix or syscall (which
// would hide the calls) and any use of SYS_FLOCK (a raw system call).
func TestNoUnlockInPackage(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		for _, finding := range flockFindings(fset, f) {
			t.Error(finding)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no Go files found")
	}
}

// TestNoUnlockDetector checks the detector itself on sources that must and must not pass.
func TestNoUnlockDetector(t *testing.T) {
	const header = "package p\nimport (\n\t\"golang.org/x/sys/unix\"\n\tsc \"syscall\"\n)\nvar _ = sc.Getpid\n"
	cases := map[string]bool{ // body -> must be refused
		"func f(fd int) error { return unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) }": false,
		"func f(fd int) error { return unix.Flock(fd, (unix.LOCK_EX)) }":            false,
		"func f(fd int) error { return sc.Flock(fd, sc.LOCK_EX|sc.LOCK_NB) }":       false,
		"func f(fd int) error { return unix.Flock(fd, unix.LOCK_UN) }":              true,
		"func f(fd int) error { return unix.Flock(fd, 8) }":                         true,
		"func f(fd int) error { return sc.Flock(fd, sc.LOCK_SH) }":                  true,
		"func f(fd, op int) error { return unix.Flock(fd, op) }":                    true,
		"func f(fd int) error { return unix.Flock(fd, unix.LOCK_EX&unix.LOCK_NB) }": true,
		"var g = unix.Flock": true,
		"func f(fd int) { unix.Syscall(unix.SYS_FLOCK, uintptr(fd), 8, 0) }": true,
		"var n = sc.SYS_FLOCK": true,
	}
	for body, refuse := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", header+body+"\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got := len(flockFindings(fset, f)) > 0; got != refuse {
			t.Errorf("%s: refused = %v, want %v", body, got, refuse)
		}
	}
	for _, src := range []string{
		"package p\nimport . \"golang.org/x/sys/unix\"\nfunc f(fd int) error { return Flock(fd, 8) }\n",
		"package p\nimport . \"syscall\"\nvar _ = Getpid\n",
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if len(flockFindings(fset, f)) == 0 {
			t.Errorf("a dot-import was not refused:\n%s", src)
		}
	}
}

// flockFindings lists the refused uses of flock in one file.
func flockFindings(fset *token.FileSet, f *ast.File) []string {
	var findings []string
	report := func(pos token.Pos, msg string) {
		findings = append(findings, fmt.Sprintf("%s: %s", fset.Position(pos), msg))
	}
	pkgs := map[string]bool{} // local names of the flock packages in this file
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if !flockPackages[p] {
			continue
		}
		name := filepath.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." {
			report(imp.Pos(), "dot-import of "+p+" hides its flock calls from this check")
			continue
		}
		pkgs[name] = true
	}
	if len(pkgs) == 0 {
		return findings
	}
	isPkgSel := func(e ast.Expr, sel ...string) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := s.X.(*ast.Ident)
		if !ok || !pkgs[id.Name] {
			return false
		}
		for _, name := range sel {
			if s.Sel.Name == name {
				return true
			}
		}
		return false
	}
	var allowed func(ast.Expr) bool
	allowed = func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.ParenExpr:
			return allowed(x.X)
		case *ast.BinaryExpr:
			return x.Op == token.OR && allowed(x.X) && allowed(x.Y)
		}
		return isPkgSel(e, "LOCK_EX", "LOCK_NB")
	}
	calls := map[ast.Expr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && isPkgSel(c.Fun, "Flock") {
			calls[c.Fun] = true
			if len(c.Args) != 2 || !allowed(c.Args[1]) {
				report(c.Pos(), "flock with an operation other than LOCK_EX/LOCK_NB: release by closing, never LOCK_UN")
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		switch {
		case !ok:
		case isPkgSel(e, "Flock") && !calls[e]:
			report(e.Pos(), "Flock used as a value; call it directly so its operation can be checked")
		case isPkgSel(e, "SYS_FLOCK"):
			report(e.Pos(), "raw flock system call; use Flock with LOCK_EX/LOCK_NB")
		}
		return true
	})
	return findings
}
