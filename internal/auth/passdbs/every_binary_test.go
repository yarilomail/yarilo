package passdbs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every binary that authenticates builds its chain here (#1861).
//
// yarilo-submission had a loop of its own that handed every entry to the SQL
// constructor, so a chain carrying static — which every other component
// accepts — killed it at startup. One config serves the whole release; a
// second reading of it is a component that refuses what its siblings allow.
// driverConstructors names every constructor passdbs.Build owns, by package: a
// list that knows only SQL guards only SQL (#1861). OAuth2 is not here -- it is
// built apart on purpose, so SQL never sees a bearer token. A new driver
// belongs in Build and in this list together.
var driverConstructors = map[string]string{
	"github.com/yarilomail/yarilo/internal/auth/sql":        "New",
	"github.com/yarilomail/yarilo/internal/auth/passwdfile": "New",
	"github.com/yarilomail/yarilo/internal/auth/static":     "New",
}

func TestNoBinaryBuildsItsOwnPassdbChain(t *testing.T) {
	mains, err := filepath.Glob("../../../app/*/main.go")
	if err != nil || len(mains) == 0 {
		t.Fatalf("no binaries found: %v", err)
	}
	for _, path := range mains {
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
