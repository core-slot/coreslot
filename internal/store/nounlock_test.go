//go:build unix

package store

import (
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
// section 6). It also refuses Flock used as a value, which would hide the operation.
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
		checkFlockCalls(t, fset, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no Go files found")
	}
}

func checkFlockCalls(t *testing.T, fset *token.FileSet, f *ast.File) {
	t.Helper()
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
		pkgs[name] = true
	}
	if len(pkgs) == 0 {
		return
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
				t.Errorf("%s: flock with an operation other than LOCK_EX/LOCK_NB: release by closing, never LOCK_UN",
					fset.Position(c.Pos()))
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok && isPkgSel(e, "Flock") && !calls[e] {
			t.Errorf("%s: Flock used as a value; call it directly so its operation can be checked",
				fset.Position(e.Pos()))
		}
		return true
	})
}
