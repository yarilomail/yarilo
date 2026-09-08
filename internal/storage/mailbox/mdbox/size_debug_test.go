package mdbox

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// capture collects the records a run logs, with their attributes.
type capture struct {
	mu   sync.Mutex
	recs []map[string]any
}

func (c *capture) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (c *capture) WithAttrs([]slog.Attr) slog.Handler           { return c }
func (c *capture) WithGroup(string) slog.Handler                { return c }
func (c *capture) Handle(_ context.Context, r slog.Record) error {
	m := map[string]any{"msg": r.Message, "level": r.Level}
	r.Attrs(func(a slog.Attr) bool { m[a.Key] = a.Value.Any(); return true })
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, m)
	return nil
}

func (c *capture) find(msg string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.recs {
		if r["msg"] == msg {
			return r
		}
	}
	return nil
}

// atLevel points the default logger at a capture and restores it after.
func atLevel(t *testing.T, level slog.Level) *capture {
	t.Helper()
	c := &capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(&levelled{h: c, min: level}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

type levelled struct {
	h   slog.Handler
	min slog.Level
}

func (l *levelled) Enabled(_ context.Context, lv slog.Level) bool { return lv >= l.min }
func (l *levelled) WithAttrs(a []slog.Attr) slog.Handler {
	return &levelled{h: l.h.WithAttrs(a), min: l.min}
}
func (l *levelled) WithGroup(g string) slog.Handler {
	return &levelled{h: l.h.WithGroup(g), min: l.min}
}
func (l *levelled) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < l.min {
		return nil
	}
	return l.h.Handle(ctx, r)
}

// sizelessMeta is a record of a saved message with its sizes taken away, which
// is the state that sends a size question to storage.
func sizelessMeta(t *testing.T, u *userMailbox, folder, filename string) *mailbox.MessageMeta {
	t.Helper()
	mapUID, saveDate, ok := u.StorageKey(folder, filename)
	if !ok {
		t.Fatalf("no storage key for %q", filename)
	}
	return &mailbox.MessageMeta{UID: 7, MapUID: mapUID, SaveDate: saveDate}
}

// A size read from storage says which frame it read: without the offset and the
// frame's length a wrong number cannot be told from a neighbour's (#1749).
func TestTheStorageSizeRowNamesTheFrameItRead(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)
	body := "From: a@a.com\r\nSubject: sized\r\n\r\nbody\r\n"
	name, _, _, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := sizelessMeta(t, u, "INBOX", name)

	c := atLevel(t, slog.LevelDebug)
	size, vsize, err := u.RecordSize("INBOX", m)
	if err != nil {
		t.Fatal(err)
	}
	row := c.find("mdbox: size read from storage, the record carried none")
	if row == nil {
		t.Fatal("a size came from storage and no row said so")
	}
	for _, k := range []string{"user", "folder", "uid", "map_uid", "size", "vsize", "file_id", "offset", "frame_size"} {
		if _, ok := row[k]; !ok {
			t.Errorf("the row carries no %q: %v", k, row)
		}
	}
	if row["user"] != "alice@example.com" {
		t.Errorf("the row names user %v, want the account the handle serves", row["user"])
	}
	if fmt.Sprint(row["size"]) != fmt.Sprint(size) || fmt.Sprint(row["vsize"]) != fmt.Sprint(vsize) {
		t.Errorf("the row says %v/%v, the call answered %d/%d", row["size"], row["vsize"], size, vsize)
	}
	if row["frame_size"] == nil || fmt.Sprint(row["frame_size"]) == "0" {
		t.Errorf("the row names frame_size %v, so a number cannot be read as a frame length", row["frame_size"])
	}
}

// At info the row is silent, and the second map lookup behind it is not made:
// a reading taken outside the gate is what the debug arc cost last time (#1740).
func TestTheStorageSizeRowCostsNothingAtInfo(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	u := openTestUserMailbox(t, home)
	body := "From: a@a.com\r\nSubject: sized\r\n\r\nbody\r\n"
	name, _, _, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := sizelessMeta(t, u, "INBOX", name)

	c := atLevel(t, slog.LevelInfo)
	ResetSizeRowLookups()
	if _, _, err := u.RecordSize("INBOX", m); err != nil {
		t.Fatal(err)
	}
	if row := c.find("mdbox: size read from storage, the record carried none"); row != nil {
		t.Errorf("info said %v", row)
	}
	if got := SizeRowLookups(); got != 0 {
		t.Errorf("the row made %d map lookups at info; a reading outside the gate is what it costs", got)
	}
}
