package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// A message body is read through the driver, which decrypts it: a function
// outside the storage that names a message file and opens a path itself
// would read ciphertext, or plaintext a store must not hold (MAILCRYPT).
func TestNoMessageFileOpenedByPath(t *testing.T) {
	fset := token.NewFileSet()
	named := 0
	for _, path := range goFiles(t, "../..") {
		name := rel(path)
		if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "internal/storage/") ||
			strings.HasPrefix(name, "pkg/mailbox/") || strings.HasPrefix(name, "internal/mailcrypt/") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			names, opens := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "MessagePath" || sel.Sel.Name == "RecordPath" {
					names = true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" &&
					(sel.Sel.Name == "Open" || sel.Sel.Name == "OpenFile" || sel.Sel.Name == "ReadFile") {
					opens = true
				}
				return true
			})
			if names {
				named++
			}
			if names && opens {
				t.Errorf("%s: %s names a message file and opens a path itself; read it through the driver", name, fn.Name.Name)
			}
		}
	}
	if named == 0 {
		t.Fatal("no function names a message file: the walk saw nothing")
	}
}

// The hardlink Copy of sdbox and mdbox shares one file between two records,
// which is right only while the keys are global; nothing calls it, and a
// caller comes with the rule for folder keys (MAILCRYPT step 4).
func TestNoCallerOfTheHardlinkCopy(t *testing.T) {
	fset := token.NewFileSet()
	for _, path := range goFiles(t, "../..") {
		name := rel(path)
		if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "internal/storage/mailbox/dboxv2/") ||
			strings.HasPrefix(name, "internal/storage/mailbox/mdbox/") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Copy" && len(call.Args) == 4 {
					t.Errorf("%s: calls a four-argument Copy, the drivers' hardlink copy", name)
				}
			}
			return true
		})
	}
}
