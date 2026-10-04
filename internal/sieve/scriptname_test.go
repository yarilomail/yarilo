package sieve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidScriptName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"plain", true},
		{"vacation rules", true},
		{"привіт", true},
		{strings.Repeat("a", 256), true},
		{strings.Repeat("a", 257), false},
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{"/../bob/.yarilo", false},
		{`a\b`, false},
		{"a\x00b", false},
		{"a\nb", false},
		{"a\x7fb", false},
		{"a\u0085b", false},
		{"aÿb", false},
		{"a b", false},
		{"a\xffb", false},
	} {
		if got := ValidScriptName(tc.name); got != tc.ok {
			t.Errorf("ValidScriptName(%q) = %v, want %v", tc.name, got, tc.ok)
		}
	}
}

// The store refuses a bad name even when no protocol layer checked it.
func TestTheFileStoreRefusesABadNameItself(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "alice")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	ss := &FsScriptStore{DefaultName: FallbackDefaultName}
	ctx := context.Background()
	bad := "/../bob/x"
	for name, err := range map[string]error{
		"SaveScript":   ss.SaveScript(ctx, "alice", home, bad, []byte("keep;")),
		"SetActive":    ss.SetActive(ctx, "alice", home, bad),
		"DeleteScript": ss.DeleteScript(ctx, "alice", home, bad),
		"RenameScript": ss.RenameScript(ctx, "alice", home, "x", bad),
	} {
		if !errors.Is(err, ErrInvalidScriptName) {
			t.Errorf("%s = %v, want ErrInvalidScriptName", name, err)
		}
	}
	if _, _, err := ss.GetScript(ctx, "alice", home, bad); !errors.Is(err, ErrInvalidScriptName) {
		t.Errorf("GetScript = %v, want ErrInvalidScriptName", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Fatalf("the mail root has %d entries, want 1", len(entries))
	}
}
