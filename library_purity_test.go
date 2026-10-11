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

// TestLibraryPurity parses every non-test Go file outside cmd/ and fails on output, process
// exit and signal handling: the library never prints (fmt.Print*, the log package), never
// chooses an exit code (os.Exit) and never installs signal handlers (signal.Notify,
// signal.Ignore).
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
		checked++
		for _, v := range violations(f) {
			t.Errorf("%s: %s", fset.Position(v.Pos()), describe(v))
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

func violations(f *ast.File) []*ast.SelectorExpr {
	names := imported(f)
	var out []*ast.SelectorExpr
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch names[x.Name] {
		case "fmt":
			if strings.HasPrefix(sel.Sel.Name, "Print") {
				out = append(out, sel)
			}
		case "log":
			out = append(out, sel)
		case "os":
			if sel.Sel.Name == "Exit" {
				out = append(out, sel)
			}
		case "os/signal":
			if sel.Sel.Name == "Notify" || sel.Sel.Name == "Ignore" {
				out = append(out, sel)
			}
		}
		return true
	})
	return out
}

func describe(sel *ast.SelectorExpr) string {
	return sel.X.(*ast.Ident).Name + "." + sel.Sel.Name + " in library code"
}
