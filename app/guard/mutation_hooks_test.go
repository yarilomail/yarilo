package guard_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const quotaPkg = "github.com/yarilomail/yarilo/pkg/quota"

// hookCeilings is each protocol package's count of direct quota, FTS and event
// calls; it only goes down, to zero once the mutation layer owns them (#2207).
var hookCeilings = map[string]int{
	"internal/imap":        0,
	"internal/lmtp":        0,
	"internal/jmap":        0,
	"internal/pop3":        0,
	"internal/submission":  0,
	"internal/managesieve": 0,
}

// ftsWrites are the mutation-side ftsproto.Service writes by arity; Prepend is
// the search catch-up, and arity keeps a transaction's Expunge(uid) out.
var ftsWrites = map[string]int{"Index": 4, "Expunge": 4, "DropFolder": 2}

// storageCeilings count direct Box saves, moves and expunges per protocol
// package; the mutation layer makes them (#2207).
var storageCeilings = map[string]int{
	"internal/imap":        0,
	"internal/lmtp":        0,
	"internal/jmap":        0,
	"internal/pop3":        0,
	"internal/submission":  0,
	"internal/managesieve": 0,
}

// storageWrites are the Box writes by arity; the layer's own calls of the same
// names take other arities.
var storageWrites = map[string]int{"Save": 7, "SaveReceived": 8, "RecordSaved": 4, "Move": 4, "ExpungeMarked": 3}

var quotaEnforcement = map[string]bool{"IsOver": true, "IsOverAny": true, "IsOverWithGrace": true}

func TestProtocolsLeaveHooksToTheMutationLayer(t *testing.T) {
	fset := token.NewFileSet()
	got := map[string]int{}
	for _, path := range goFiles(t, "../..") {
		name := rel(path)
		pkg := filepath.Dir(name)
		if _, watched := hookCeilings[pkg]; !watched || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got[pkg] += hookCalls(f)
	}
	for _, pkg := range sortedKeys(hookCeilings) {
		switch n, ceiling := got[pkg], hookCeilings[pkg]; {
		case n > ceiling:
			t.Errorf("%s calls quota, FTS or events directly %d times, ceiling %d: go through the mutation layer (#2207)", pkg, n, ceiling)
		case n < ceiling:
			t.Errorf("%s is down to %d direct hook calls: lower its ceiling from %d", pkg, n, ceiling)
		}
	}
}

func TestHookCallsCountsEachShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"quota enforcement", `q.IsOver(u, l, 1, 1); q.IsOverAny(u, l); q.IsOverWithGrace(u, l, 1, 1, 0)`, 3},
		{"quota reporting", `q.CountUsage(b, nil, l)`, 0},
		{"same name off the quota package", `x.IsOver(u, l, 1, 1)`, 0},
		{"fts writes", `c.Index(u, m, 1, 0); c.Expunge(u, m, 1, g); c.DropFolder(u, m)`, 3},
		{"fts search catch-up", `c.Prepend(u, m, 1)`, 0},
		{"fts read", `c.Lookup(u, m, q)`, 0},
		{"transaction expunge", `tx.Expunge(uid)`, 0},
		{"layer expunge", `s.ops.ExpungeMarked(b, f, n, m)`, 0},
		{"event", `l.Emit(ctx, k, t, p)`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p\nimport q \"" + quotaPkg + "\"\nvar _ = q.Limits{}\nfunc f() {\n" + tc.body + "\n}\n"
			f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if n := hookCalls(f); n != tc.want {
				t.Errorf("counted %d, want %d", n, tc.want)
			}
		})
	}
}

// hookCalls counts quota enforcement under any import name of pkg/quota, FTS
// writes by name and arity, and four-argument Emit, the locks.Locker shape.
func hookCalls(f *ast.File) int {
	quotaAlias := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == quotaPkg {
			quotaAlias = filepath.Base(p)
			if imp.Name != nil {
				quotaAlias = imp.Name.Name
			}
		}
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		if id, ok := sel.X.(*ast.Ident); ok && quotaAlias != "" && id.Name == quotaAlias && quotaEnforcement[name] {
			n++
		}
		if arity, ok := ftsWrites[name]; ok && len(call.Args) == arity {
			n++
		}
		if name == "Emit" && len(call.Args) == 4 {
			n++
		}
		return true
	})
	return n
}

func TestProtocolsSaveThroughTheMutationLayer(t *testing.T) {
	fset := token.NewFileSet()
	got := map[string]int{}
	for _, path := range goFiles(t, "../..") {
		name := rel(path)
		pkg := filepath.Dir(name)
		if _, watched := storageCeilings[pkg]; !watched || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got[pkg] += callsByArity(f, storageWrites)
	}
	for _, pkg := range sortedKeys(storageCeilings) {
		switch n, ceiling := got[pkg], storageCeilings[pkg]; {
		case n > ceiling:
			t.Errorf("%s saves, moves or expunges past the mutation layer %d times, ceiling %d (#2207)", pkg, n, ceiling)
		case n < ceiling:
			t.Errorf("%s is down to %d direct storage writes: lower its ceiling from %d", pkg, n, ceiling)
		}
	}
}

func TestCallsByArityCountsEachShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"box writes", `b.Save(f, r, 0, n, fl, kw, g); b.SaveReceived(f, r, 0, n, fl, kw, g, d); b.RecordSaved(fo, f, s, m); b.Move(f, d, s, g); b.ExpungeMarked(fo, f, ms)`, 5},
		{"layer save", `s.ops.Save(b, f, fo, req)`, 0},
		{"layer expunge", `s.ops.ExpungeMarked(b, fo, f, ms)`, 0},
		{"protocol move", `t.s.Move(w, set, dest)`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), "p.go", "package p\nfunc f() {\n"+tc.body+"\n}\n", parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if n := callsByArity(f, storageWrites); n != tc.want {
				t.Errorf("counted %d, want %d", n, tc.want)
			}
		})
	}
}

// callsByArity counts selector calls whose name and argument count match.
func callsByArity(f *ast.File, shapes map[string]int) int {
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if arity, ok := shapes[sel.Sel.Name]; ok && len(call.Args) == arity {
					n++
				}
			}
		}
		return true
	})
	return n
}
