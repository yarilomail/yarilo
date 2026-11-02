package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// fts_search_read_fallback reaches Email/query from the file: its default,
// and an explicit false kept (#2480).
func TestFTSSearchTakesTheReadFallback(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{"left out", "", true},
		{"off", "fts:\n  fts_search_read_fallback: false\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "yarilo.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := ftsSearch(cfg, nil, nil).ReadFallback; got != tc.want {
				t.Errorf("ReadFallback = %v, want %v", got, tc.want)
			}
		})
	}
}
