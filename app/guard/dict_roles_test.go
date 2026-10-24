package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mtls"
)

// dictDialers are the exported backend builders that reach the dict service,
// found by following calls to buildDict through the package.
func dictDialers(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "../../internal/backend", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string][]string{}
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok {
						if id, ok := c.Fun.(*ast.Ident); ok {
							calls[fn.Name.Name] = append(calls[fn.Name.Name], id.Name)
						}
					}
					return true
				})
			}
		}
	}
	reaches := map[string]bool{"buildDict": true}
	for changed := true; changed; {
		changed = false
		for fn, callees := range calls {
			if reaches[fn] {
				continue
			}
			for _, c := range callees {
				if reaches[c] {
					reaches[fn], changed = true, true
					break
				}
			}
		}
	}
	out := map[string]bool{}
	for fn := range reaches {
		if ast.IsExported(fn) {
			out[fn] = true
		}
	}
	return out
}

// Every binary that opens a dict through the dict service presents a role the
// dict listener accepts; a refused role loses its writes quietly (quota clone).
func TestEveryDictClientIsAcceptedByTheDictListener(t *testing.T) {
	dialers := dictDialers(t)
	if !dialers["New"] || !dialers["QuotaServices"] {
		t.Fatalf("dict builders %v; the walk lost buildDict", dialers)
	}
	accepted := mtls.Allowed(mtls.ListenerDict)
	dirs, err := filepath.Glob("../../app/yarilo-*")
	if err != nil {
		t.Fatal(err)
	}
	var clients []string
	for _, dir := range dirs {
		bin := filepath.Base(dir)
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			dials := false
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "backend" && dialers[sel.Sel.Name] {
						dials = true
					}
				}
				return !dials
			})
			if !dials {
				continue
			}
			role := mtls.Role(strings.TrimPrefix(bin, "yarilo-"))
			if !slices.Contains(mtls.Roles, role) {
				t.Errorf("%s opens a dict but is not an mtls role", bin)
			} else if !slices.Contains(accepted, role) {
				t.Errorf("%s opens a dict through the dict service, which refuses role %q", bin, role)
			}
			clients = append(clients, bin)
			break
		}
	}
	sort.Strings(clients)
	if len(clients) < 5 {
		t.Fatalf("dict clients %v; the scan looks at nothing", clients)
	}
}
