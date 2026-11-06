package sieve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// A user script that does not run keeps the message in INBOX, unless a
// global-before script cancelled the keep (#2530).
func TestSkippedUserScriptKeeps(t *testing.T) {
	cases := []struct {
		name   string
		script string
		before string
		want   []string
	}{
		{"does not compile", `this is not sieve`, "", []string{"INBOX"}},
		{"forbidden extension", `require "vnd.yarilo.pipe"; keep;`, "", []string{"INBOX"}},
		{"global before filed it", `this is not sieve`, `require "fileinto"; fileinto "Spam";`, []string{"Spam"}},
		{"global before kept it", `this is not sieve`, `keep;`, []string{"INBOX"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.SieveConfig{Enabled: true, MaxRedirects: 32, MaxScriptSize: 65536,
				DefaultName: FallbackDefaultName, SieveExtensions: []string{"fileinto"}}
			if c.before != "" {
				p := filepath.Join(t.TempDir(), "before.sieve")
				if err := os.WriteFile(p, []byte(c.before), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg.GlobalBefore = []string{p}
			}
			e := New(cfg, nil, nil, nil)
			store := newTestStore()
			home := t.TempDir()
			ctx := context.Background()
			if err := store.SaveScript(ctx, "u1", home, "test", []byte(c.script)); err != nil {
				t.Fatal(err)
			}
			if err := store.SetActive(ctx, "u1", home, "test"); err != nil {
				t.Fatal(err)
			}
			result, err := e.Filter(ctx, baseOpts("u1", home))
			if err != nil {
				t.Fatalf("filter: %v", err)
			}
			var got []string
			for _, d := range result.Deliveries {
				got = append(got, d.Folder)
			}
			if len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0]) {
				t.Errorf("deliveries %v, want %v", got, c.want)
			}
		})
	}
}
