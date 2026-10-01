package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const mailboxPkg = "github.com/yarilomail/yarilo/pkg/mailbox"

// storageSide are the packages that are part of the storage and may declare the
// index or the store in their own signatures (#1805).
var storageSide = []string{"internal/msgcache/", "internal/ftsservice/", "internal/backendapi/", "app/yarilo-migrate/", "internal/ftsbench/"}

// notYetMoved is what still reaches past Box, per file; it only shrinks, each
// PR of #1805 taking its sites off.
var notYetMoved = map[string]int{
	"internal/lmtp/deliver.go": 2,
}

// Protocol servers see Box and nothing else: a new consumer of the index or of
// Box.Store() outside the storage side is refused (#1805).
func TestOnlyTheStorageSideHoldsTheIndexOrTheStore(t *testing.T) {
	fset := token.NewFileSet()
	got := map[string]int{}
	allowed := 0
	for _, path := range goFiles(t, "../..") {
		name := rel(path)
		if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "internal/storage/") || strings.HasPrefix(name, "pkg/mailbox/") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		n := reachesPastBox(f)
		if n == 0 {
			continue
		}
		if onStorageSide(name) {
			allowed += n
			continue
		}
		got[name] = n
	}
	if allowed == 0 {
		t.Fatal("the storage side holds no index at all, so the walk saw nothing")
	}
	for _, name := range sortedKeys(got, notYetMoved) {
		if got[name] != notYetMoved[name] {
			t.Errorf("%s reaches past Box %d times, the list says %d: protocol servers see Box alone (#1805)", name, got[name], notYetMoved[name])
		}
	}
}

// reachesPastBox counts mailbox.UserIndex under any import name, and calls of
// Store() with no arguments, the shape of Box.Store().
func reachesPastBox(f *ast.File) int {
	alias := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == mailboxPkg {
			alias = filepath.Base(p)
			if imp.Name != nil {
				alias = imp.Name.Name
			}
		}
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && alias != "" && id.Name == alias && x.Sel.Name == "UserIndex" {
				n++
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Store" && len(x.Args) == 0 {
				n++
			}
		}
		return true
	})
	return n
}

func onStorageSide(name string) bool {
	for _, p := range storageSide {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func sortedKeys(maps ...map[string]int) []string {
	seen := map[string]bool{}
	for _, m := range maps {
		for k := range m {
			seen[k] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
