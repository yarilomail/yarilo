package sieve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// sieve_global_extensions at delivery: the administrator's scripts get them,
// a user's never do, and an include takes its location's set (#2290).
func TestGlobalExtensionsAtDelivery(t *testing.T) {
	cases := []struct {
		name    string
		exts    []string          // sieve_extensions
		global  []string          // sieve_global_extensions
		before  string            // a sieve_global_before script; "" for none
		site    string            // <sieve_global>/site.sieve; "" for none
		scripts map[string]string // the user's; "main" is active
		folders []string
		err     string
	}{
		{name: "a before script uses a global-only extension",
			exts: []string{"envelope"}, global: []string{"fileinto"},
			before:  `require "fileinto"; fileinto "Site";`,
			folders: []string{"Site"}},
		{name: "a before script beyond both lists is left out",
			exts: []string{"envelope"}, global: []string{"envelope"},
			before:  `require "fileinto"; fileinto "Site";`,
			scripts: map[string]string{"main": `keep;`},
			folders: []string{"INBOX"}},
		{name: "ihave in a before script answers from the administrator's set",
			exts: []string{"ihave"}, global: []string{"fileinto"},
			before:  `require ["ihave", "fileinto"]; if ihave "editheader" { fileinto "Wide"; } fileinto "Site";`,
			folders: []string{"Site"}},
		{name: "a user's script may not have it",
			exts: []string{"fileinto"}, global: []string{"fileinto"},
			scripts: map[string]string{"main": `require "fileinto"; fileinto "User";`},
			folders: nil},
		{name: "ihave in a user's script is false",
			exts: []string{"ihave"}, global: []string{"fileinto"},
			scripts: map[string]string{"main": `require "ihave"; if ihave "fileinto" { fileinto "User"; }`},
			folders: []string{"INBOX"}},
		{name: "a :global include from a user's script gets it",
			exts: []string{"include"}, global: []string{"fileinto"},
			site:    `require "fileinto"; fileinto "Site";`,
			scripts: map[string]string{"main": `require "include"; include :global "site";`},
			folders: []string{"Site"}},
		{name: "ihave in a :global include answers from the administrator's set",
			exts: []string{"include", "ihave"}, global: []string{"fileinto"},
			site:    `require "ihave"; if ihave "fileinto" { fileinto "Site"; }`,
			scripts: map[string]string{"main": `require "include"; include :global "site";`},
			folders: []string{"Site"}},
		{name: "a :personal include using it fails the run",
			exts: []string{"include"}, global: []string{"fileinto"},
			scripts: map[string]string{"main": `require "include"; include :personal "inner";`, "inner": `require "fileinto"; fileinto "User";`},
			err:     "fileinto"},
		{name: "a :personal include from a before script answers ihave from the user's set",
			exts: []string{"include", "ihave"}, global: []string{"fileinto"},
			before:  `require "include"; include :personal "mine";`,
			scripts: map[string]string{"mine": `require "ihave"; if ihave "fileinto" { fileinto "Leak"; }`},
			folders: []string{"INBOX"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := config.SieveConfig{Enabled: true, MaxRedirects: 32, MaxScriptSize: 65536, DefaultName: FallbackDefaultName,
				SieveExtensions: c.exts, GlobalExtensions: c.global, Global: dir}
			if c.before != "" {
				p := filepath.Join(dir, "before.sieve")
				if err := os.WriteFile(p, []byte(c.before), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg.GlobalBefore = []string{p}
			}
			if c.site != "" {
				if err := os.WriteFile(filepath.Join(dir, "site.sieve"), []byte(c.site), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			e := New(cfg, nil, nil, nil)
			store, home, ctx := newTestStore(), t.TempDir(), context.Background()
			for name, src := range c.scripts {
				if err := store.SaveScript(ctx, "u1", home, name, []byte(src)); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := c.scripts["main"]; ok {
				if err := store.SetActive(ctx, "u1", home, "main"); err != nil {
					t.Fatal(err)
				}
			}
			res, err := e.Filter(ctx, baseOpts("u1", home))
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want one naming %s", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			if res != nil {
				for _, d := range res.Deliveries {
					got = append(got, d.Folder)
				}
			}
			if strings.Join(got, ",") != strings.Join(c.folders, ",") {
				t.Errorf("deliveries %v, want %v", got, c.folders)
			}
		})
	}
}

// An imapsieve script is the administrator's: it gets the global-only ones.
func TestGlobalExtensionsInIMAPEvent(t *testing.T) {
	dir, home, ctx := t.TempDir(), t.TempDir(), context.Background()
	if err := os.WriteFile(filepath.Join(dir, "s.sieve"), []byte(`require ["imapsieve", "fileinto"]; fileinto "Seen";`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newImapEngine(t, dir)
	e.cfg.SieveExtensions, e.cfg.GlobalExtensions = []string{"imapsieve"}, []string{"fileinto"}
	res, err := e.RunIMAPEvent(ctx, IMAPEventOptions{
		Username: "u1", HomeDir: home, Cause: "COPY", Mailbox: "Spam", MsgRaw: []byte("Subject: hi\r\n\r\nb\r\n"), ScriptName: "s",
	})
	if err != nil || res == nil || len(res.Deliveries) != 1 || res.Deliveries[0].Folder != "Seen" {
		t.Fatalf("RunIMAPEvent = %+v, %v; want fileinto Seen", res, err)
	}
}
