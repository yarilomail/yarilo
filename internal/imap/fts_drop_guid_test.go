package imap

import (
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type dropRecorder struct {
	mu      sync.Mutex
	dropped []string
}

func (d *dropRecorder) DropFolder(_ string, m fts.MailboxRef) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropped = append(d.dropped, m.Name)
	return nil
}

func (d *dropRecorder) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dropped...)
}

func (d *dropRecorder) Index(string, fts.MailboxRef, uint32, int) error        { return nil }
func (d *dropRecorder) Prepend(string, fts.MailboxRef, uint32) error           { return nil }
func (d *dropRecorder) Expunge(string, fts.MailboxRef, uint32, [16]byte) error { return nil }
func (d *dropRecorder) Lookup(string, fts.MailboxRef, fts.Query) (fts.Result, error) {
	return fts.Result{}, nil
}
func (d *dropRecorder) Status(string, fts.MailboxRef) (uint32, uint32, error) { return 0, 0, nil }
func (d *dropRecorder) Rescan(string, fts.MailboxRef) error                   { return nil }
func (d *dropRecorder) RescanUser(string) ([]string, error)                   { return nil, nil }
func (d *dropRecorder) Counts(string) (uint64, uint64, uint64, uint64, error) { return 0, 0, 0, 0, nil }
func (d *dropRecorder) LookupIn(string, []fts.MailboxRef, fts.Query) (fts.SetResult, error) {
	return fts.SetResult{}, nil
}
func (d *dropRecorder) Optimize(string) error { return nil }
func (d *dropRecorder) Close() error          { return nil }

// The hex of an all-zero GUID is not empty, so only the bytes tell a folder
// without identity from one with it.
func TestDropFolderSkipsAFolderWithoutGUID(t *testing.T) {
	rec := &dropRecorder{}
	s := &session{
		srv:      &Server{opts: Options{FTS: FTSOptions{Client: rec}}},
		userInfo: &mailbox.UserInfo{Username: "u1@example.com"},
	}
	s.ftsDropFolder(&mailbox.Folder{Name: "NoGUID"})
	s.ftsDropFolder(&mailbox.Folder{Name: "Kept", GUID: [16]byte{1}})

	deadline := time.Now().Add(3 * time.Second)
	for len(rec.seen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := rec.seen(); len(got) != 1 || got[0] != "Kept" {
		t.Errorf("dropped %v, want [Kept]", got)
	}
}
