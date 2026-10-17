package sieve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

// include runs a script of the user's own at delivery (RFC 6609, #2282).
func TestIncludeAtDelivery(t *testing.T) {
	cases := []struct {
		name    string
		exts    []string          // sieve_extensions; nil allows all
		scripts map[string]string // "main" is active
		folders []string          // fileinto targets, in order; nil with err
		err     string            // a substring of Filter's error
	}{
		{name: "a personal script's actions run",
			scripts: map[string]string{"main": `require "include"; include :personal "inner";`, "inner": `require "fileinto"; fileinto "Inner";`},
			folders: []string{"Inner"}},
		{name: "no location is personal",
			scripts: map[string]string{"main": `require "include"; include "inner";`, "inner": `require "fileinto"; fileinto "Inner";`},
			folders: []string{"Inner"}},
		{name: ":once includes once",
			scripts: map[string]string{
				"main":  `require ["include", "variables", "fileinto"]; global "n"; include :once "inner"; include :once "inner"; if string "${n}" "x" { fileinto "Once"; }`,
				"inner": `require ["include", "variables"]; global "n"; set "n" "${n}x";`},
			folders: []string{"Once"}},
		{name: ":optional skips a missing script",
			scripts: map[string]string{"main": `require ["include", "fileinto"]; include :optional "nosuch"; fileinto "After";`},
			folders: []string{"After"}},
		{name: "a missing script fails the run",
			scripts: map[string]string{"main": `require ["include", "fileinto"]; include "nosuch"; fileinto "After";`},
			err:     `"nosuch"`},
		{name: "recursion is refused",
			scripts: map[string]string{"main": `require "include"; include "inner";`, "inner": `require "include"; include "inner";`},
			err:     "recursive"},
		{name: "an included script is held to sieve_extensions", exts: []string{"include", "fileinto"},
			scripts: map[string]string{"main": `require "include"; include "inner";`, "inner": `require "imap4flags"; addflag "\\Flagged";`},
			err:     "imap4flags"},
		{name: ":global finds nothing yet",
			scripts: map[string]string{"main": `require ["include", "fileinto"]; include :global :optional "site"; fileinto "After";`},
			folders: []string{"After"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := New(config.SieveConfig{Enabled: true, MaxRedirects: 32, MaxScriptSize: 65536, DefaultName: FallbackDefaultName, SieveExtensions: c.exts}, nil, nil, nil)
			store := newTestStore()
			home, ctx := t.TempDir(), context.Background()
			for name, src := range c.scripts {
				if err := store.SaveScript(ctx, "u1", home, name, []byte(src)); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.SetActive(ctx, "u1", home, "main"); err != nil {
				t.Fatal(err)
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
			for _, d := range res.Deliveries {
				got = append(got, d.Folder)
			}
			if strings.Join(got, ",") != strings.Join(c.folders, ",") {
				t.Errorf("deliveries %v, want %v", got, c.folders)
			}
		})
	}
}

// An imapsieve script includes the user's own the same way.
func TestIncludeInIMAPEvent(t *testing.T) {
	dir, home, ctx := t.TempDir(), t.TempDir(), context.Background()
	if err := os.WriteFile(filepath.Join(dir, "report.sieve"), []byte(`require ["imapsieve", "include"]; include :personal "mine";`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newTestStore().SaveScript(ctx, "u1", home, "mine", []byte(`require "fileinto"; fileinto "Mine";`)); err != nil {
		t.Fatal(err)
	}
	res, err := newImapEngine(t, dir).RunIMAPEvent(ctx, IMAPEventOptions{
		Username: "u1", HomeDir: home, Cause: "COPY", Mailbox: "Spam", MsgRaw: []byte("Subject: hi\r\n\r\nb\r\n"), ScriptName: "report",
	})
	if err != nil || res == nil || len(res.Deliveries) != 1 || res.Deliveries[0].Folder != "Mine" {
		t.Fatalf("RunIMAPEvent = %+v, %v; want fileinto Mine", res, err)
	}
}
