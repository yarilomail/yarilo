package guard_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The repo's comments are English and name no other mail server by product
// name. Both rules were kept by review alone and slipped three times (#1900).
func TestCommentsAreEnglishAndNameNoProduct(t *testing.T) {
	// On-disk names are what they are: a file called dovecot-uidlist is a file
	// name, not a mention of the product that writes it.
	onDisk := regexp.MustCompile(`dovecot(-uidlist|\.index(\.cache|\.log)?|-keywords|\.mailbox\.log|\.list\.index|-acl|\.sieve|\.conf)`)
	product := regexp.MustCompile(`(?i)\b(dovecot|doveadm|dsync|doveconf)\b`)

	files := goFiles(t, "../..")
	if len(files) < 200 {
		t.Fatalf("walked %d go files, which cannot be right", len(files))
	}
	fset := token.NewFileSet()
	checked := 0
	for _, path := range files {
		// The parser tells a comment from a string literal, block comments
		// included; reading the lines ourselves does not.
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", rel(path), err)
		}
		for _, group := range f.Comments {
			for _, c := range group.List {
				checked++
				where := fset.Position(c.Pos())
				if hasCyrillicOutsideQuotes(c.Text) {
					t.Errorf("%s:%d: the comment itself is not English: %s", rel(path), where.Line, firstLine(c.Text))
				}
				if stripped := onDisk.ReplaceAllString(c.Text, ""); product.MatchString(stripped) {
					t.Errorf("%s:%d: the comment names another product: %s", rel(path), where.Line, firstLine(c.Text))
				}
			}
		}
	}
	if checked < 1000 {
		t.Fatalf("read %d comments, which cannot be right", checked)
	}
}

// hasCyrillicOutsideQuotes ignores quoted data: an English sentence about the
// folder "Вхідні" is a comment about a fixture, not a comment in Ukrainian.
func hasCyrillicOutsideQuotes(comment string) bool {
	quoted := false
	for _, r := range comment {
		switch {
		case r == '"' || r == '`':
			quoted = !quoted
		case !quoted && unicode.Is(unicode.Cyrillic, r):
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func rel(path string) string { return strings.TrimPrefix(path, "../../") }
