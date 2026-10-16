package dboxconv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxconv"
)

// Their FTS index carries index files of its own; counted as an unconverted
// folder it keeps their map on disk forever (#2254).
func TestTheFTSIndexIsNoFolderLeftToConvert(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  bool
	}{
		{"only the FTS index holds their index files", []string{
			"INBOX/fts-flatcurve/dovecot.index.log",
			"INBOX/fts-flatcurve/dovecot.index.cache",
		}, false},
		{"a real folder beside it is still found", []string{
			"INBOX/fts-flatcurve/dovecot.index.log",
			"INBOX/Receipts/dovecot.index.log",
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range tc.files {
				p := filepath.Join(root, f)
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := dboxconv.AnyForeignFolderLeft(root)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("AnyForeignFolderLeft = %v, want %v", got, tc.want)
			}
		})
	}
}
