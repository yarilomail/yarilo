package passdbs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every binary builds its chain here: one config serves the release, and a
// second reading of it refuses what the siblings allow (#1861).

// driverConstructors names every constructor passdbs.Build owns, by package.
// OAuth2 is not here: it is built apart so SQL never sees a bearer token.
var driverConstructors = map[string]string{
	"github.com/yarilomail/yarilo/internal/auth/sql":        "New",
	"github.com/yarilomail/yarilo/internal/auth/passwdfile": "New",
	"github.com/yarilomail/yarilo/internal/auth/static":     "New",
}

func TestNoBinaryBuildsItsOwnPassdbChain(t *testing.T) {
	// Every file of every binary, not only main.go: a chain built in a second
	// file of the same package walks past a guard that reads one name.
	sources, err := filepath.Glob("../../../app/*/*.go")
	if err != nil || len(sources) == 0 {
		t.Fatalf("no binaries found: %v", err)
	}
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		// The local name each driver package goes by in this file, alias or not.
		byName := map[string]string{}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			ctor, ok := driverConstructors[p]
			if !ok {
				continue
			}
			name := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			byName[name] = ctor
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if ctor, guarded := byName[pkg.Name]; guarded && sel.Sel.Name == ctor {
				t.Errorf("%s calls %s.%s directly; a chain built by hand knows only the drivers it lists (#1861)",
					strings.TrimPrefix(path, "../../../"), pkg.Name, ctor)
			}
			return true
		})
	}
}
