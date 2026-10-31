package mailops

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/quotawarn"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
	"github.com/yarilomail/yarilo/v2/pkg/quota"
)

// overStatusUser is an account with a one-message limit holding n messages,
// whose userdb carries flag, with quota_over_status set to run "over".
func overStatusUser(t *testing.T, flag string, n int, lazy bool) (*Ops, string) {
	t.Helper()
	bin := t.TempDir()
	out := filepath.Join(t.TempDir(), "over.out")
	// Written aside and renamed, so the row never reads a file half written.
	script := "#!/bin/sh\necho \"$QUOTA_OVER $QUOTA_OVER_FLAG\" > " + out + ".tmp && mv " + out + ".tmp " + out + "\n"
	if err := os.WriteFile(filepath.Join(bin, "over"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	info := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir(), Driver: "maildir",
		QuotaRules: []string{"*:messages=1"}, QuotaOverFlag: flag}
	store := maildir.New().OpenUser(info)
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	box := mailboxbase.Open(store, idx)
	// The policy as the loader builds it: the percentages default to 100.
	policy := config.Defaults().Quota.QuotaPolicy()
	policy.OverStatus = quota.OverStatusPolicy{Mask: "TRUE", LazyCheck: lazy, Execute: "over"}
	cfg := Config{Protocol: "test", Quota: QuotaConfig{Policy: policy, Warner: quotawarn.New(bin, 5)}}
	o := New(cfg, box, info)
	f, err := box.Folder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		body := "Subject: m\r\n\r\nmessage " + strings.Repeat("x", i) + "\r\n"
		if _, err := o.Save(box, "INBOX", f, SaveRequest{Body: strings.NewReader(body), Size: int64(len(body)), QuotaChecked: true}); err != nil {
			t.Fatal(err)
		}
	}
	return New(cfg, box, info), out
}

func logged(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&b, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &b
}

// At login the userdb over-flag is checked against the account: a mismatch runs
// the program with the real state, a match runs nothing.
func TestLoginSyncsTheOverQuotaFlag(t *testing.T) {
	for name, c := range map[string]struct {
		flag  string
		n     int
		fires string
	}{
		"over, not flagged":  {"", 2, "yes "},
		"under, flagged":     {"TRUE", 0, "no TRUE"},
		"over, flagged":      {"TRUE", 2, ""},
		"under, not flagged": {"", 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			o, out := overStatusUser(t, c.flag, c.n, false)
			log := logged(t)
			o.Quota.LoginOverStatus()
			fired := strings.Contains(log.String(), "quota over-status changed")
			if fired != (c.fires != "") {
				t.Fatalf("fired=%v, want %v", fired, c.fires != "")
			}
			if c.fires == "" {
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				got, err := os.ReadFile(out)
				if err == nil {
					if strings.TrimSpace(string(got)) != strings.TrimSpace(c.fires) {
						t.Errorf("the program saw %q, want %q", got, c.fires)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("the program did not run")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// A lazy check waits for the first quota question, and asks only once.
func TestALazyOverStatusCheckWaitsForTheFirstQuotaQuestion(t *testing.T) {
	o, _ := overStatusUser(t, "", 2, true)
	log := logged(t)
	o.Quota.LoginOverStatus()
	if strings.Contains(log.String(), "over-status") {
		t.Fatal("a lazy check ran at login")
	}
	for range 2 {
		if _, err := o.Quota.Usage("test", false); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(log.String(), "quota over-status changed"); n != 1 {
		t.Errorf("the check ran %d times, want once", n)
	}
}
