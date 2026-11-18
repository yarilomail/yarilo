package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// maildir takes a folder's uidlist file lock before the process mutex, never
// inside it (#2184, MAILCRYPT step 3): a call that reaches the list from
// within the mutex meets a path taking them the other way round, and the two
// wait out uidListLockWait on each other.
func TestMaildirTakesTheListBeforeTheMutex(t *testing.T) {
	takers := map[string]bool{
		"holdList": true, "dotlock": true, "SaveSection": true, "HoldFolder": true,
		"Remove": true, "WriteFlags": true, "WriteFlagsMulti": true, "Move": true,
		"seedUIDValidity": true, "RewriteRecord": true, "AssignUID": true,
	}
	mutexes := map[string]bool{"withMailboxLockSite": true, "withTwoMailboxLocks": true}
	fset := token.NewFileSet()
	closures := 0
	for _, path := range goFiles(t, "../..") {
		name := rel(path)
		if !strings.HasPrefix(name, "internal/storage/mailbox/maildir/") || strings.HasSuffix(name, "_test.go") {
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
			if !ok || !mutexes[sel.Sel.Name] {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.FuncLit)
				if !ok {
					continue
				}
				closures++
				ast.Inspect(lit.Body, func(n ast.Node) bool {
					inner, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if s, ok := inner.Fun.(*ast.SelectorExpr); ok && takers[s.Sel.Name] {
						if id, ok := s.X.(*ast.Ident); ok && id.Name == "u" {
							t.Errorf("%s:%d: %s reaches the list inside %s: the list is taken before the mutex",
								name, fset.Position(inner.Pos()).Line, s.Sel.Name, sel.Sel.Name)
						}
					}
					return true
				})
			}
			return true
		})
	}
	if closures == 0 {
		t.Fatal("no mutex closure in maildir: the walk saw nothing")
	}
}
