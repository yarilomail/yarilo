package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// A namespace other than the user's own mail is opened by internal/storage/nsopen
// alone: two openers are how IMAP and the admin API came to disagree (#1109, #2598).
func TestOnlyTheNamespaceOpenerDescribesANamespace(t *testing.T) {
	builders := map[string]bool{"NamespaceUserInfo": true, "StampOwnerLocation": true}
	fset := token.NewFileSet()
	seen := 0
	for _, path := range goFiles(t, "../..") {
		name := rel(path)
		if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "pkg/mailbox/") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !builders[sel.Sel.Name] {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "mailbox" {
				return true
			}
			if strings.HasPrefix(name, "internal/storage/nsopen/") {
				seen++
				return true
			}
			t.Errorf("%s:%d: mailbox.%s outside internal/storage/nsopen; open the namespace through it",
				name, fset.Position(call.Pos()).Line, sel.Sel.Name)
			return true
		})
	}
	if seen < len(builders) {
		t.Fatalf("the opener calls %d of the %d builders: the walk saw nothing", seen, len(builders))
	}
}
