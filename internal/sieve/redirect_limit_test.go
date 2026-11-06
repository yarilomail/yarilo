package sieve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// sieve_max_redirects counts every redirect of a message, :copy and :list
// included, merges a repeat of a target and fails the run over the limit
// (#2529).
func TestRedirectLimit(t *testing.T) {
	cases := []struct {
		name    string
		max     int
		script  string
		wantErr bool
		want    []string
	}{
		{"copy counts", 1, `require "copy"; redirect :copy "a@example.org"; redirect :copy "b@example.org";`, true, nil},
		{"copy then plain counts", 1, `require "copy"; redirect :copy "a@example.org"; redirect "b@example.org";`, true, nil},
		{"repeat with copy is merged", 1, `require "copy"; redirect :copy "a@example.org"; redirect :copy "a@EXAMPLE.org";`, false, []string{"a@example.org"}},
		{"repeat is merged", 1, `redirect "a@example.org"; redirect "a@EXAMPLE.org";`, false, []string{"a@example.org"}},
		{"repeat with another local part is not", 1, `redirect "a@example.org"; redirect "A@example.org";`, true, nil},
		{"list counts", 1, `require "extlists"; redirect "a@example.org"; redirect :list "team";`, true, nil},
		{"at the limit", 2, `redirect "a@example.org"; redirect "b@example.org";`, false, []string{"a@example.org", "b@example.org"}},
		{"one over the limit fails", 2, `redirect "a@example.org"; redirect "b@example.org"; redirect "c@example.org";`, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := New(config.SieveConfig{Enabled: true, MaxRedirects: c.max, MaxScriptSize: 65536,
				DefaultName: FallbackDefaultName}, nil, nil, nil)
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
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), "exceeds policy limit") {
					t.Fatalf("err %v, want the policy limit", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("filter: %v", err)
			}
			var got []string
			for _, r := range result.Redirects {
				got = append(got, r.Address)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("redirects %v, want %v", got, c.want)
			}
		})
	}
}

// The limit is per message, over every script that runs on it.
func TestRedirectLimitSpansScripts(t *testing.T) {
	before := filepath.Join(t.TempDir(), "before.sieve")
	if err := os.WriteFile(before, []byte(`redirect "a@example.org";`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := New(config.SieveConfig{Enabled: true, MaxRedirects: 1, MaxScriptSize: 65536,
		DefaultName: FallbackDefaultName, GlobalBefore: []string{before}}, nil, nil, nil)
	store := newTestStore()
	home := t.TempDir()
	ctx := context.Background()
	if err := store.SaveScript(ctx, "u1", home, "test", []byte(`redirect "b@example.org";`)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, "u1", home, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Filter(ctx, baseOpts("u1", home)); err == nil || !strings.Contains(err.Error(), "exceeds policy limit") {
		t.Fatalf("err %v, want the policy limit", err)
	}
}

// sieve_max_redirects 0 refuses a script with redirect anywhere in it.
func TestCheckRedirects(t *testing.T) {
	cases := []struct {
		name   string
		max    int
		script string
		want   error
	}{
		{"zero refuses redirect", 0, `redirect "a@example.org";`, ErrRedirectProhibited},
		{"zero refuses a nested one", 0, `if true { if false { REDIRECT "a@example.org"; } }`, ErrRedirectProhibited},
		{"zero takes a script without", 0, `keep;`, nil},
		{"one takes redirect", 1, `redirect "a@example.org";`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := CheckRedirects([]byte(c.script), c.max); !errors.Is(err, c.want) {
				t.Errorf("err %v, want %v", err, c.want)
			}
		})
	}
}

// At delivery a stored script with redirect is skipped under 0: implicit keep.
func TestFilterSkipsRedirectScriptAtZero(t *testing.T) {
	e := New(config.SieveConfig{Enabled: true, MaxRedirects: 0, MaxScriptSize: 65536,
		DefaultName: FallbackDefaultName}, nil, nil, nil)
	store := newTestStore()
	home := t.TempDir()
	ctx := context.Background()
	if err := store.SaveScript(ctx, "u1", home, "test", []byte(`redirect "a@example.org"; discard;`)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActive(ctx, "u1", home, "test"); err != nil {
		t.Fatal(err)
	}
	result, err := e.Filter(ctx, baseOpts("u1", home))
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if result == nil || len(result.Redirects) != 0 || len(result.Deliveries) != 1 || result.Deliveries[0].Folder != "INBOX" {
		t.Fatalf("result %+v, want implicit keep only", result)
	}
}
