package config_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// sessionBinaries are the processes a login proxy fronts. The shape lived in
// five copies and the fifth did not have it, which is how #1863 was found.
var sessionBinaries = []string{"imap", "pop3", "lmtp", "managesieve", "submission"}

// Each of them asks config for its listener, and none writes the services or
// reads the server certificate itself (#1863).
func TestEverySessionBinaryKeepsOnlyItsListener(t *testing.T) {
	for _, name := range sessionBinaries {
		files, err := filepath.Glob("../../app/yarilo-" + name + "/*.go")
		if err != nil || len(files) == 0 {
			t.Fatalf("no sources for yarilo-%s: %v", name, err)
		}
		calls, writes, tlsReads := 0, 0, 0
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
						if sel.Sel.Name == "KeepOnlySessionListener" {
							calls++
						}
						// BuildTLSConfig on general.ssl is the login proxy's
						// job; a backend reading it dies on a missing file.
						if sel.Sel.Name == "BuildTLSConfig" && len(node.Args) == 1 && generalSSL(node.Args[0]) {
							tlsReads++
						}
					}
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						if servicesField(lhs) {
							writes++
						}
					}
				}
				return true
			})
		}
		if calls != 1 {
			t.Errorf("yarilo-%s calls KeepOnlySessionListener %d times, want 1", name, calls)
		}
		if writes != 0 {
			t.Errorf("yarilo-%s assigns cfg.Services.* %d times; the shape belongs in one place (#1863)", name, writes)
		}
		if tlsReads != 0 {
			t.Errorf("yarilo-%s builds TLS from general.ssl %d times; the proxy in front holds that certificate (#1863)", name, tlsReads)
		}
	}
}

// servicesField answers for an expression like cfg.Services.IMAPS.
func servicesField(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "Services"
}

// generalSSL answers for cfg.General.SSL passed whole.
func generalSSL(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "SSL"
}
