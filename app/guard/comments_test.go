package guard_test

import (
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
	onDisk := regexp.MustCompile(`dovecot(-uidlist|\.index|\.index\.cache|\.index\.log|-keywords|\.mailbox\.log|\.list\.index|-acl|\.sieve)`)
	product := regexp.MustCompile(`(?i)\b(dovecot|doveadm|dsync|doveconf)\b`)

	files := goFiles(t, "../..")
	if len(files) < 200 {
		t.Fatalf("walked %d go files, which cannot be right", len(files))
	}
	for _, path := range files {
		for n, line := range strings.Split(readFile(t, path), "\n") {
			comment, ok := commentOf(line)
			if !ok {
				continue
			}
			if hasCyrillicOutsideQuotes(comment) {
				t.Errorf("%s:%d: the comment itself is not English: %s", rel(path), n+1, strings.TrimSpace(comment))
			}
			if stripped := onDisk.ReplaceAllString(comment, ""); product.MatchString(stripped) {
				t.Errorf("%s:%d: the comment names another product: %s", rel(path), n+1, strings.TrimSpace(comment))
			}
		}
	}
}

// commentOf returns the comment part of a line, ignoring a // that sits inside
// a string literal, which is what a wire format or a URL looks like.
func commentOf(line string) (string, bool) {
	inStr, inRune := false, false
	for i := 0; i < len(line)-1; i++ {
		switch {
		case line[i] == '\\':
			i++
		case line[i] == '"' && !inRune:
			inStr = !inStr
		case line[i] == '\'' && !inStr:
			inRune = !inRune
		case line[i] == '/' && line[i+1] == '/' && !inStr && !inRune:
			return line[i+2:], true
		}
	}
	return "", false
}

// hasCyrillicOutsideQuotes ignores quoted data: an English sentence about the
// folder "Вхідні" is a comment about a fixture, not a comment in Ukrainian.
func hasCyrillicOutsideQuotes(comment string) bool {
	var b strings.Builder
	quoted := false
	for _, r := range comment {
		if r == '"' || r == '`' {
			quoted = !quoted
			continue
		}
		if !quoted {
			b.WriteRune(r)
		}
	}
	for _, r := range b.String() {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && (info.Name() == ".git" || info.Name() == "vendor") {
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
