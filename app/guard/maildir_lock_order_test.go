package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// maildir takes the crypt lock, then the uidlist lock, then the process mutex
// (#2184, MAILCRYPT section 13.5): a call that reaches either file lock from
// within the mutex, or the crypt lock from within a save's list hold, meets a
// path taking them the other way round, and the two wait out their bounds.
func TestMaildirTakesTheFileLocksBeforeTheMutex(t *testing.T) {
	takers := map[string]bool{
		"holdList": true, "dotlock": true, "SaveSection": true, "HoldFolder": true,
		"Remove": true, "WriteFlags": true, "WriteFlagsMulti": true, "Move": true,
		"seedUIDValidity": true, "RewriteRecord": true, "AssignUID": true,
		"sharedCrypt": true, "exclusiveCrypt": true, "RestoreMoved": true,
	}
	mutexes := map[string]bool{"withMailboxLockSite": true, "withTwoMailboxLocks": true, "SaveSection": true}
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
							t.Errorf("%s:%d: %s reaches a file lock inside %s: the file locks are taken before it",
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
