package sieve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// ihave answers from sieve_extensions at delivery: a block under an
// extension the list leaves out never runs, one it allows does (#2297).
func TestIhaveAtDeliveryHoldsToSieveExtensions(t *testing.T) {
	const script = `require "ihave"; if ihave "fileinto" { fileinto "Bypass"; }`
	for _, c := range []struct {
		allowed []string
		want    string
	}{
		{[]string{"ihave"}, "INBOX"},
		{[]string{"ihave", "fileinto"}, "Bypass"},
	} {
		e := New(config.SieveConfig{Enabled: true, MaxRedirects: 32, MaxScriptSize: 65536,
			DefaultName: FallbackDefaultName, SieveExtensions: c.allowed}, nil, nil, nil)
		store, home, ctx := newTestStore(), t.TempDir(), context.Background()
		if err := store.SaveScript(ctx, "u1", home, "t", []byte(script)); err != nil {
			t.Fatal(err)
		}
		if err := store.SetActive(ctx, "u1", home, "t"); err != nil {
			t.Fatal(err)
		}
		r, err := e.Filter(ctx, baseOpts("u1", home))
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Deliveries) != 1 || r.Deliveries[0].Folder != c.want {
			t.Errorf("sieve_extensions %v: delivered to %+v, want %s", c.allowed, r.Deliveries, c.want)
		}
	}
}

// An imapsieve script with a block under a refused extension runs the rest:
// the block is dead, not a reason to refuse the script (#2297).
func TestIhaveInIMAPEventHoldsToSieveExtensions(t *testing.T) {
	dir, home, ctx := t.TempDir(), t.TempDir(), context.Background()
	src := `require ["imapsieve", "ihave", "fileinto"]; if ihave "editheader" { addheader "X" "y"; } fileinto "Seen";`
	if err := os.WriteFile(filepath.Join(dir, "s.sieve"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newImapEngine(t, dir)
	e.cfg.SieveExtensions = []string{"imapsieve", "ihave", "fileinto"}
	res, err := e.RunIMAPEvent(ctx, IMAPEventOptions{
		Username: "u1", HomeDir: home, Cause: "COPY", Mailbox: "Spam", MsgRaw: []byte("Subject: hi\r\n\r\nb\r\n"), ScriptName: "s",
	})
	if err != nil || res == nil || len(res.Deliveries) != 1 || res.Deliveries[0].Folder != "Seen" {
		t.Fatalf("RunIMAPEvent = %+v, %v; want fileinto Seen", res, err)
	}
}
