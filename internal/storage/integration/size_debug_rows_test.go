package integration_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type rowCapture struct {
	mu   sync.Mutex
	min  slog.Level
	recs []map[string]any
}

func (c *rowCapture) Enabled(_ context.Context, lv slog.Level) bool { return lv >= c.min }
func (c *rowCapture) WithAttrs([]slog.Attr) slog.Handler            { return c }
func (c *rowCapture) WithGroup(string) slog.Handler                 { return c }
func (c *rowCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Level < c.min {
		return nil
	}
	m := map[string]any{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool { m[a.Key] = a.Value.Any(); return true })
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, m)
	return nil
}

func (c *rowCapture) find(msg string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.recs {
		if r["msg"] == msg {
			return r
		}
	}
	return nil
}

func captureAt(t *testing.T, level slog.Level) *rowCapture {
	t.Helper()
	c := &rowCapture{min: level}
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// A record appended with no virtual size names the site that appended it: every
// save path holds one by then, so the site is the whole answer (#1741).
func TestASizelessAppendNamesItsSite(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "erin@example.com", Home: home}
	idx := file.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	c := captureAt(t, slog.LevelDebug)
	if err := idx.AllocateAndAppend(f.ID, &mailbox.MessageMeta{Size: 40}); err != nil {
		t.Fatal(err)
	}
	row := c.find("fileindex: appended a record with no virtual size")
	if row == nil {
		t.Fatal("a record went in with no virtual size and no row said so")
	}
	for _, k := range []string{"trace_id", "user", "folder", "uid", "size", "map_uid", "site"} {
		if _, ok := row[k]; !ok {
			t.Errorf("the row carries no %q: %v", k, row)
		}
	}
	if row["user"] != "erin@example.com" {
		t.Errorf("the row names user %v", row["user"])
	}
	if site := fmt.Sprint(row["site"]); !strings.Contains(site, "AllocateAndAppend") && !strings.Contains(site, "integration") {
		t.Errorf("the row names site %q, which does not say where the record came from", site)
	}
}

// The same append says nothing at info.
func TestASizelessAppendIsSilentAtInfo(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "erin@example.com", Home: home}
	idx := file.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	c := captureAt(t, slog.LevelInfo)
	if err := idx.AllocateAndAppend(f.ID, &mailbox.MessageMeta{Size: 40}); err != nil {
		t.Fatal(err)
	}
	if row := c.find("fileindex: appended a record with no virtual size"); row != nil {
		t.Errorf("info said %v", row)
	}
}

// The stamping row says how many records had no size and which ones were given
// one, so a slot reads who filled them in and when (#1741).
func TestTheStampingRowNamesWhatItFilled(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "frank@example.com", Home: home}
	box := mdbox.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := file.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	const body = "From: a@b\r\nSubject: stamped\r\n\r\nbody\r\n"
	saved, _, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{GUID: guid}
	if err := mailbox.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}

	c := captureAt(t, slog.LevelDebug)
	n, err := mailbox.FillSizelessRecords(idx, box, f)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Skip("nothing was sizeless, so there is nothing for the row to say")
	}
	row := c.find("mailbox: records carried no size; stamping what storage answers")
	if row == nil {
		t.Fatal("sizes were stamped and no row said so")
	}
	for _, k := range []string{"user", "folder", "sizeless", "stamping", "uids"} {
		if _, ok := row[k]; !ok {
			t.Errorf("the row carries no %q: %v", k, row)
		}
	}
	if row["user"] != "frank@example.com" {
		t.Errorf("the row names user %v, want the account the handle serves", row["user"])
	}
}
