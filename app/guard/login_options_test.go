package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// Each listener of a login binary is configured by its own login.Options
// literal; a field set on one and not the other is a setting that port ignores.
func TestEveryLoginListenerGetsTheSameSettings(t *testing.T) {
	tlsOnly := map[string]bool{"ExtTLS": true, "StarttlsTLS": true}
	for _, bin := range []string{"imap", "pop3", "submission", "managesieve"} {
		path := "../yarilo-" + bin + "-login/main.go"
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var sets []string
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if se, ok := cl.Type.(*ast.SelectorExpr); !ok || se.Sel.Name != "Options" ||
				se.X.(*ast.Ident).Name != "login" {
				return true
			}
			var keys []string
			for _, e := range cl.Elts {
				if k := e.(*ast.KeyValueExpr).Key.(*ast.Ident).Name; !tlsOnly[k] {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			sets = append(sets, strings.Join(keys, " "))
			return true
		})
		if len(sets) == 0 {
			t.Fatalf("%s: no login.Options literal found", path)
		}
		for i, s := range sets {
			if !strings.Contains(" "+s+" ", " ProxyTimeout ") {
				t.Errorf("%s: listener %d does not set ProxyTimeout", path, i)
			}
			if s != sets[0] {
				t.Errorf("%s: listener %d sets\n  %s\nlistener 0 sets\n  %s", path, i, s, sets[0])
			}
		}
	}
}
