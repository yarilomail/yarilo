package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// writeTree lays files out under a fresh root and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const prose = `package a

// one
// two
// three
func F() {}

// short
// pair
func G() {}
`

// Only prose of the minimum length reaches the work list; machinery and
// licence blocks are counted as comment but never listed, and test files,
// testdata and vendor are not walked at all.
func TestWalkListsOnlyProseBlocks(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a/a.go": prose,
		"a/machinery.go": `//go:build linux
// +build linux
// nolint stays

package a
`,
		"a/licence.go": `// Copyright 2026 somebody
// all rights
// reserved

package a
`,
		"b/cgo.go": `package b

// #include <stdio.h>
// #include <stdlib.h>
// #include <string.h>
import "C"
`,
		"a/a_test.go":     "package a\n\n// x\n// y\n// z\nfunc T() {}\n",
		"a/testdata/t.go": "package t\n\n// x\n// y\n// z\n",
		"vendor/v/v.go":   "package v\n\n// x\n// y\n// z\n",
		"c/broken.go":     "package c\n\nfunc {\n",
		"c/notes.txt":     "// x\n// y\n// z\n",
	})
	census, blocks, err := walk(root, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Line != 3 || blocks[0].Lines != 3 || blocks[0].First != "one" ||
		!strings.HasSuffix(blocks[0].File, "a/a.go") {
		t.Fatalf("blocks = %+v, want only a/a.go:3, 3 lines, first \"one\"", blocks)
	}
	got := map[string]PackageCensus{}
	for _, c := range census {
		got[strings.TrimPrefix(c.Package, filepath.ToSlash(root)+"/")] = c
	}
	if len(got) != 3 {
		t.Fatalf("packages = %v, want a, b and c only", got)
	}
	a := got["a"]
	if a.GoLines != 10+5+5 || a.CommentLines != 5+3+3 || a.Blocks != 1 || a.BlockLines != 3 {
		t.Errorf("a = %+v, want 20 Go lines, 11 comment lines, one block of 3", a)
	}
	if b := got["b"]; b.CommentLines != 3 || b.Blocks != 0 {
		t.Errorf("b = %+v, want the cgo preamble counted and not listed", b)
	}
	if c := got["c"]; c.GoLines != 3 || c.CommentLines != 0 {
		t.Errorf("c = %+v, want an unparsable file counted for its lines only", c)
	}
}

// -pkg narrows the walk: "x/..." is x and everything under it, as in Go, and
// a plain prefix is just that.
func TestWalkFollowsThePackagePrefix(t *testing.T) {
	root := writeTree(t, map[string]string{"x/a.go": prose, "x/sub/a.go": prose, "xy/a.go": prose})
	rel := func(p string) string { return filepath.ToSlash(filepath.Join(root, p)) }
	tests := []struct {
		pattern string
		want    []string
	}{
		{"x/...", []string{"x", "x/sub"}},
		{"x", []string{"x", "x/sub", "xy"}},
		{"x/sub", []string{"x/sub"}},
	}
	for _, tc := range tests {
		census, _, err := walk(root, rel(tc.pattern), 3)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range census {
			got = append(got, strings.TrimPrefix(c.Package, rel("")+"/"))
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("-pkg %s: %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

// The gate: a swept package is held to the threshold, an unswept one to what
// the baseline recorded, and either fails the moment it exceeds it.
func TestRunCheckHoldsEachPackageToItsLimit(t *testing.T) {
	base := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(base, []byte(`{"old": 0.2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pkg := func(name string, blockLines int) PackageCensus {
		return PackageCensus{Package: name, GoLines: 100, BlockLines: blockLines, Blocks: 1}
	}
	tests := []struct {
		name   string
		census []PackageCensus
		want   bool
	}{
		{"swept at the threshold", []PackageCensus{pkg("new", 8)}, true},
		{"swept over the threshold", []PackageCensus{pkg("new", 9)}, false},
		{"unswept at its baseline", []PackageCensus{pkg("old", 20)}, true},
		{"unswept grown past its baseline", []PackageCensus{pkg("old", 21)}, false},
		{"a package with no Go lines", []PackageCensus{{Package: "empty"}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := runCheck(base, tc.census, 0.08); got != tc.want {
				t.Errorf("runCheck = %v, want %v", got, tc.want)
			}
		})
	}
	if runCheck(filepath.Join(t.TempDir(), "absent.json"), []PackageCensus{pkg("old", 21)}, 0.08) {
		t.Error("without a baseline every package is held to the threshold")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runCheck(bad, nil, 0.08) {
		t.Error("an unreadable baseline passed the gate")
	}
}

// The baseline records only packages over the threshold, rounded up a hair,
// and reads back as it was written.
func TestBaselineRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	census := []PackageCensus{
		{Package: "over", GoLines: 1000, BlockLines: 123},
		{Package: "at", GoLines: 100, BlockLines: 8},
		{Package: "empty"},
	}
	if err := writeBaseline(path, census, 0.08); err != nil {
		t.Fatal(err)
	}
	got, err := readBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]float64{"over": 0.124}; !reflect.DeepEqual(got, want) {
		t.Errorf("baseline = %v, want %v", got, want)
	}
}

func TestRoundUpLeavesRoom(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{{0, 0.001}, {0.1234, 0.124}, {0.2, 0.201}} {
		if got := roundUp(tc.in); got != tc.want {
			t.Errorf("roundUp(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCountLinesAndFirstLine(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want int
	}{{"", 0}, {"a", 1}, {"a\n", 1}, {"a\nb", 2}, {"a\nb\n", 2}} {
		if got := countLines([]byte(tc.src)); got != tc.want {
			t.Errorf("countLines(%q) = %d, want %d", tc.src, got, tc.want)
		}
	}
	root := writeTree(t, map[string]string{"p/p.go": "package p\n\n/* first line\n   second\n   third */\nfunc F() {}\n"})
	_, blocks, err := walk(root, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].First != "first line" || blocks[0].Lines != 3 {
		t.Errorf("blocks = %+v, want one /* */ block of 3 lines titled \"first line\"", blocks)
	}
}

// The dash scan reads only what git tracks, skips links, and honours the
// allowance file; an allowance it cannot read stops it.
func TestEmDashesScansTrackedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := writeTree(t, map[string]string{
		"bad.go":       "package x\n// one " + emDash + " two\n",
		"good.go":      "package x\n// one - two\n",
		"data/w.txt":   "a " + emDash + " b\n",
		"untracked.go": "// " + emDash + "\n",
	})
	if err := os.Symlink("bad.go", filepath.Join(root, "link.go")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "bad.go", "good.go", "data/w.txt", "link.go"}} {
		if out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	allow := filepath.Join(t.TempDir(), "allow.json")
	body, _ := json.Marshal([]dashAllowance{{File: "data/"}})
	if err := os.WriteFile(allow, body, 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := emDashes(root, allow)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bad.go:2: // one " + emDash + " two"}; !reflect.DeepEqual(found, want) {
		t.Errorf("found = %q, want %q", found, want)
	}

	for _, body := range []string{"", "not json"} {
		p := filepath.Join(t.TempDir(), "allow.json")
		if body != "" {
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := emDashes(root, p); err == nil {
			t.Errorf("allowance %q: no error", body)
		}
	}
}
