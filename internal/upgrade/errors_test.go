package upgrade

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoErrorIsDroppedSilently is the package's fail-open class held whole
// (spec 054 #37): an error the command drops can be read as "absent", "not
// started", "nothing" — and a decision made on it. Every error the
// package's own code drops — a blank assignment, or a call whose error is
// not taken — says why on its line, `// ignored: <why>`, and the why is one
// a reviewer can check: the value is only a message's, a no answer is
// refused below, a cleanup of what is the command's own. Errors read and
// acted on need nothing.
func TestNoErrorIsDroppedSilently(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var parsed []*ast.File
	// Functions of the package that answer an error, by name.
	returnsErr := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Type.Results != nil {
				for _, r := range fd.Type.Results.List {
					if id, ok := r.Type.(*ast.Ident); ok && id.Name == "error" {
						returnsErr[fd.Name.Name] = true
					}
				}
			}
		}
	}
	// The standard library's calls the package makes for their effect.
	for _, n := range []string{"Remove", "RemoveAll", "Rename", "Chmod", "Chtimes", "Mkdir", "MkdirAll", "WriteFile", "Symlink", "Link", "Close", "Kill", "Signal", "Sync", "Encode", "WriteString",
		"Readlink", "ReadFile", "Getwd", "LookPath", "Read", "ReadString", "Version", "Executable", "UserHomeDir"} {
		returnsErr[n] = true
	}
	called := func(e ast.Expr) string {
		switch f := e.(type) {
		case *ast.Ident:
			return f.Name
		case *ast.SelectorExpr:
			return f.Sel.Name
		}
		return ""
	}
	seen := 0
	for _, f := range parsed {
		reason := map[int]bool{}
		for _, g := range f.Comments {
			for _, c := range g.List {
				if strings.Contains(c.Text, "ignored: ") {
					reason[fset.Position(c.Pos()).Line] = true
				}
			}
		}
		explained := func(n ast.Node) bool {
			line := fset.Position(n.Pos()).Line
			return reason[line] || reason[line-1]
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.DeferStmt:
				return false // a deferred Close of what was read
			case *ast.AssignStmt:
				blank, taken := false, false
				for _, l := range s.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						blank = blank || id.Name == "_"
						taken = taken || id.Name == "err" || strings.HasSuffix(id.Name, "Err")
					}
				}
				if !blank || taken || len(s.Rhs) != 1 {
					break
				}
				if c, ok := s.Rhs[0].(*ast.CallExpr); ok && (returnsErr[called(c.Fun)] || len(s.Lhs) == 1) {
					seen++
					if !explained(s) {
						t.Errorf("%s: %s's error dropped without `// ignored: <why>`", fset.Position(s.Pos()), called(c.Fun))
					}
				}
			case *ast.ExprStmt:
				if c, ok := s.X.(*ast.CallExpr); ok && returnsErr[called(c.Fun)] {
					seen++
					if !explained(s) {
						t.Errorf("%s: %s's error not taken, without `// ignored: <why>`", fset.Position(s.Pos()), called(c.Fun))
					}
				}
			}
			return true
		})
	}
	if seen == 0 {
		t.Fatal("found no dropped error at all; the check would pass anything")
	}
	if _, err := os.Stat("errors_test.go"); err != nil {
		t.Fatal(err)
	}
}
