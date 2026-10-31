package mailops

import (
	"errors"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// fullAccount holds one message against a one-message limit, with Spam left
// out of the count; the engine enforces.
func fullAccount(t *testing.T, rules ...string) (Config, mailbox.Box, *mailbox.UserInfo) {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir(), Driver: "maildir", QuotaRules: rules}
	store := maildir.New().OpenUser(info)
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	box := mailboxbase.Open(store, idx)
	cfg := Config{Protocol: "test", Quota: QuotaConfig{Engine: true, Policy: config.Defaults().Quota.QuotaPolicy()}}
	o := New(cfg, box, info)
	body := "Subject: m\r\n\r\nfirst\r\n"
	if _, err := o.Save(box, "INBOX", nil, SaveRequest{Body: strings.NewReader(body), Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"Spam"} {
		if err := box.Create(f); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, box, info
}

// A folder the rules leave out takes mail on a full account, from a session and
// from a delivery alike; any other folder is refused.
func TestAnIgnoredFolderTakesMailOnAFullAccount(t *testing.T) {
	cfg, box, info := fullAccount(t, "*:messages=1", "Spam:ignore")
	for name, o := range map[string]*Ops{"session": New(cfg, box, info), "delivery": NewDelivery(cfg, box, info)} {
		body := "Subject: s\r\n\r\nspam\r\n"
		if _, err := o.Save(box, "Spam", nil, SaveRequest{Body: strings.NewReader(body), Size: int64(len(body))}); err != nil {
			t.Errorf("%s: save into the ignored folder: %v", name, err)
		}
		var refused *RefusedError
		if err := o.Quota.CheckSave("INBOX", int64(len(body))); !errors.As(err, &refused) {
			t.Errorf("%s: a save into INBOX on a full account: %v", name, err)
		}
	}
}

// An account with no limits takes any upload without counting anything.
func TestAnUnlimitedAccountTakesAnyUpload(t *testing.T) {
	cfg, box, info := fullAccount(t)
	if err := New(cfg, box, info).Quota.CheckUpload(1 << 40); err != nil {
		t.Errorf("an unlimited account refused an upload: %v", err)
	}
}

// A session with no user behind it, as a rebuild runs, has no limits and
// counts nothing: every quota question answers "no limit" without error.
func TestASessionWithoutAUserHasNoLimits(t *testing.T) {
	q := New(Config{Protocol: "test"}, nil, nil).Quota
	if !q.Limits().Unlimited() {
		t.Error("a session with no user carries limits")
	}
	if u, err := q.Usage("test", false); err != nil || u.StorageBytes != 0 || u.Messages != 0 {
		t.Errorf("usage %+v, %v without a box", u, err)
	}
	if err := q.CheckUpload(1 << 40); err != nil {
		t.Errorf("upload refused without a user: %v", err)
	}
}
