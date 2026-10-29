package virtual

import (
	"strings"
	"testing"
)

// The rule and the \Recent claim of a folder come from the first positive line
// naming it: a negative line and an annotation line before it take no part.
func TestTheFirstPositiveLineNamesAFoldersRule(t *testing.T) {
	cfg, err := ParseConfig(strings.NewReader(strings.Join([]string{
		"-Trash",
		"/private/vendor/x:yes",
		"+INBOX",
		"Work/*",
		"  UNSEEN",
		"*",
		"  FLAGGED",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	for folder, want := range map[string]string{"INBOX": "UNSEEN", "Work/a": "UNSEEN", "Lists": "FLAGGED", "Trash": "FLAGGED"} {
		if got := ruleFor(cfg, folder); got != want {
			t.Errorf("%s takes rule %q, want %q", folder, got, want)
		}
	}
	if !cfg.ClearsRecent("INBOX") || cfg.ClearsRecent("Work/a") {
		t.Error("only the + line clears \\Recent")
	}
	empty, err := ParseConfig(strings.NewReader("-Trash\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ruleFor(empty, "Trash") != "" || empty.ClearsRecent("Trash") {
		t.Error("a negative line alone gave a folder a rule")
	}
}

// A pattern written with the client's separator reads with ours; with ours
// already, or none, it is left as written.
func TestAPatternTakesOurSeparator(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"Work.Projects.*", "."}: "Work/Projects/*",
		{"Work/Projects", "/"}:   "Work/Projects",
		{"Work", ""}:             "Work",
	} {
		if got := SameSeparator(in[0], in[1]); got != want {
			t.Errorf("SameSeparator(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
