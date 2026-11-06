package sieve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// One implicit keep for the message, settled after the sequence: a script
// that cancels it ends the sequence, and a user script that cannot run ends it
// with the keep standing (#2530).
func TestSkippedUserScriptKeeps(t *testing.T) {
	cases := []struct {
		name   string
		script string
		before string
		after  string
		want   []string
	}{
		{"does not compile", `this is not sieve`, "", "", []string{"INBOX"}},
		{"forbidden extension", `require "vnd.yarilo.pipe"; keep;`, "", "", []string{"INBOX"}},
		{"global before filed it", `this is not sieve`, `require "fileinto"; fileinto "Spam";`, "", []string{"Spam"}},
		{"global before kept it", `this is not sieve`, `keep;`, "", []string{"INBOX"}},
		{"global before discarded it", `this is not sieve`, `discard;`, "", nil},
		{"before without a match, user files it", `require "fileinto"; fileinto "User";`, `require "fileinto"; if false { fileinto "Spam"; }`, "", []string{"User"}},
		{"before files it, user does not run", `require "fileinto"; fileinto "User";`, `require "fileinto"; fileinto "Spam";`, "", []string{"Spam"}},
		{"skipped user ends the sequence before after", `this is not sieve`, "", `require "fileinto"; fileinto "After";`, []string{"INBOX"}},
		{"user without a match, after files it", `require "fileinto"; if false { fileinto "User"; }`, "", `require "fileinto"; fileinto "After";`, []string{"After"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.SieveConfig{Enabled: true, MaxRedirects: 32, MaxScriptSize: 65536,
				DefaultName: FallbackDefaultName, SieveExtensions: []string{"fileinto"}}
			for _, g := range []struct {
				src string
				to  *[]string
			}{{c.before, &cfg.GlobalBefore}, {c.after, &cfg.GlobalAfter}} {
				if g.src == "" {
					continue
				}
				p := filepath.Join(t.TempDir(), "global.sieve")
				if err := os.WriteFile(p, []byte(g.src), 0o600); err != nil {
					t.Fatal(err)
				}
				*g.to = []string{p}
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
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("deliveries %v, want %v", got, c.want)
			}
		})
	}
}
