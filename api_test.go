package coreslot

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// exportedAPI lists the root package's exported identifiers, method signatures and exported
// struct fields as the files built for goos declare them, one sorted line each.
func exportedAPI(t *testing.T, goos string) []string {
	t.Helper()
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = goos, "amd64", false
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	set := map[string]bool{}
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if ok, err := ctx.MatchFile(".", name); err != nil || !ok {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			for _, line := range declLines(d) {
				set[line] = true
			}
		}
	}
	// An alias exposes its target's exported fields: pin them too.
	for line := range set {
		alias, target, ok := strings.Cut(strings.TrimPrefix(line, "type "), " = ")
		if !ok || !strings.HasPrefix(line, "type ") {
			continue
		}
		for _, f := range aliasFields(t, fset, alias, target) {
			set[f] = true
		}
	}
	lines := make([]string, 0, len(set))
	for l := range set {
		lines = append(lines, l)
	}
	slices.Sort(lines)
	return lines
}

func declLines(d ast.Decl) []string {
	var out []string
	switch d := d.(type) {
	case *ast.FuncDecl:
		if !d.Name.IsExported() {
			return nil
		}
		sig := signature(d.Type)
		if d.Recv == nil {
			return []string{"func " + d.Name.Name + sig}
		}
		recv := types.ExprString(d.Recv.List[0].Type)
		if !ast.IsExported(strings.TrimPrefix(recv, "*")) {
			return nil
		}
		return []string{"method (" + recv + ") " + d.Name.Name + sig}
	case *ast.GenDecl:
		for _, s := range d.Specs {
			switch s := s.(type) {
			case *ast.TypeSpec:
				if !s.Name.IsExported() {
					continue
				}
				switch {
				case s.Assign.IsValid():
					out = append(out, "type "+s.Name.Name+" = "+types.ExprString(s.Type))
				default:
					if st, ok := s.Type.(*ast.StructType); ok {
						out = append(out, "type "+s.Name.Name+" struct")
						for _, f := range st.Fields.List {
							for _, n := range f.Names {
								if n.IsExported() {
									out = append(out, "field "+s.Name.Name+"."+n.Name+" "+types.ExprString(f.Type))
								}
							}
						}
					} else {
						out = append(out, "type "+s.Name.Name+" "+types.ExprString(s.Type))
					}
				}
			case *ast.ValueSpec:
				for _, n := range s.Names {
					if n.IsExported() {
						out = append(out, d.Tok.String()+" "+n.Name)
					}
				}
			}
		}
	}
	return out
}

// signature prints a function type with parameter and result types only.
func signature(ft *ast.FuncType) string {
	list := func(fl *ast.FieldList) []string {
		var ts []string
		if fl == nil {
			return nil
		}
		for _, f := range fl.List {
			n := max(1, len(f.Names))
			for i := 0; i < n; i++ {
				ts = append(ts, types.ExprString(f.Type))
			}
		}
		return ts
	}
	s := "(" + strings.Join(list(ft.Params), ", ") + ")"
	switch res := list(ft.Results); len(res) {
	case 0:
	case 1:
		s += " " + res[0]
	default:
		s += " (" + strings.Join(res, ", ") + ")"
	}
	return s
}

// TestExportedAPIGolden pins the public API in api/v0.txt, and requires the Unix and the
// Windows builds to export exactly the same API. CORESLOT_UPDATE_API=1 rewrites the golden file.
func TestExportedAPIGolden(t *testing.T) {
	unix := exportedAPI(t, "linux")
	if darwin := exportedAPI(t, "darwin"); !slices.Equal(unix, darwin) {
		t.Errorf("darwin exports differ from linux:\n%s\nvs\n%s", strings.Join(darwin, "\n"), strings.Join(unix, "\n"))
	}
	if windows := exportedAPI(t, "windows"); !slices.Equal(unix, windows) {
		t.Errorf("windows exports differ from unix:\nwindows:\n%s\nunix:\n%s", strings.Join(windows, "\n"), strings.Join(unix, "\n"))
	}
	got := strings.Join(unix, "\n") + "\n"
	golden := filepath.Join("api", "v0.txt")
	if os.Getenv("CORESLOT_UPDATE_API") == "1" {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the exported API differs from %s (CORESLOT_UPDATE_API=1 rewrites it after review):\ngot:\n%swant:\n%s", golden, got, want)
	}
}

// aliasFields lists the exported fields of an alias's target struct, "pkg.Name" with pkg one of
// the module's internal packages, as fields of the alias.
func aliasFields(t *testing.T, fset *token.FileSet, alias, target string) []string {
	t.Helper()
	pkg, name, ok := strings.Cut(target, ".")
	if !ok {
		return nil
	}
	files, err := filepath.Glob(filepath.Join("internal", pkg, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != name {
				return true
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				for _, fl := range st.Fields.List {
					for _, n := range fl.Names {
						if n.IsExported() {
							out = append(out, "field "+alias+"."+n.Name+" "+types.ExprString(fl.Type))
						}
					}
				}
			}
			return false
		})
	}
	return out
}
