package sieve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// An IMAP event runs the same sequence as delivery: one keep, deferred, and a
// script that cancels it ends the sequence (#2530).
func TestIMAPEventSequence(t *testing.T) {
	cases := []struct {
		name          string
		before, bound string
		want          []string
	}{
		{"before files it, bound does not run", `require "fileinto"; fileinto "Spam";`, `require "fileinto"; fileinto "Bound";`, []string{"Spam"}},
		{"before keeps, bound files it", `keep;`, `require "fileinto"; fileinto "Bound";`, []string{"Bound"}},
		{"bound does not compile", `keep;`, `this is not sieve`, []string{"INBOX"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "bound.sieve"), []byte(c.bound), 0o600); err != nil {
				t.Fatal(err)
			}
			before := filepath.Join(t.TempDir(), "before.sieve")
			if err := os.WriteFile(before, []byte(c.before), 0o600); err != nil {
				t.Fatal(err)
			}
			e := New(config.SieveConfig{Enabled: true, MaxRedirects: 32, MaxActions: 32, MaxScriptSize: 65536,
				DefaultName: FallbackDefaultName, ImapSieveEnabled: true, ImapSieveScriptDir: dir,
				ImapSieveGlobalBefore: []string{before}}, nil, nil, nil)
			res, err := e.RunIMAPEvent(context.Background(), IMAPEventOptions{Username: "u@x.com", Cause: "APPEND",
				Mailbox: "Work", MsgRaw: []byte("Subject: hi\r\n\r\nbody\r\n"), ScriptName: "bound"})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range res.Deliveries {
				got = append(got, d.Folder)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("deliveries %v, want %v", got, c.want)
			}
		})
	}
}
