package mailboxmetrics_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func writeFailures(t *testing.T, driver, reason string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "mailbox_write_failed_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelIs(m, "driver", driver) && labelIs(m, "reason", reason) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// A full volume refuses the body, which is a driver's write and not the
// journal's: every driver a delivery can land on has to name the class, or the
// commonest failure is the one nobody classified (#1831).
func TestEveryDriverClassifiesARefusedWrite(t *testing.T) {
	drivers := []struct {
		name string
		open func(info *mailbox.UserInfo) mailbox.UserMailbox
	}{
		{name: "maildir", open: func(i *mailbox.UserInfo) mailbox.UserMailbox { return maildir.New().OpenUser(i) }},
		{name: "sdbox", open: func(i *mailbox.UserInfo) mailbox.UserMailbox { return dboxv2.New().OpenUser(i) }},
		{name: "mdbox", open: func(i *mailbox.UserInfo) mailbox.UserMailbox { return mdbox.New().OpenUser(i) }},
	}
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			home := t.TempDir()
			box := d.open(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: d.name})
			defer box.Close() //nolint:errcheck
			if err := box.Create("INBOX"); err != nil && !strings.Contains(err.Error(), "exist") {
				t.Fatalf("create: %v", err)
			}
			// A file where the driver needs a directory: the filesystem refuses
			// the same write a full volume would, at the same seam.
			blocked := blockStorage(t, home, d.name)

			was := writeFailures(t, d.name, "other")
			_, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 0, 0, nil, nil, [16]byte{})
			if err == nil {
				t.Fatalf("the save succeeded with %s unusable", blocked)
			}
			if now := writeFailures(t, d.name, "other"); now != was+1 {
				t.Errorf("counter for %s = %v, want %v: the refusal reached no seam", d.name, now, was+1)
			}
			if errors.Is(err, mailbox.ErrNoSpace) {
				t.Errorf("a refused path was classified as a full volume: %v", err)
			}
		})
	}
}

// blockStorage puts a regular file where the driver's save needs a directory
// and returns the path it took.
func blockStorage(t *testing.T, home, driver string) string {
	t.Helper()
	var path string
	switch driver {
	case "maildir":
		path = filepath.Join(home, "Maildir", "tmp")
	case "sdbox":
		path = filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")
	case "mdbox":
		path = filepath.Join(home, "mdbox", "storage")
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("clear %s: %v", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("parent of %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block %s: %v", path, err)
	}
	return path
}

// The class a driver produced is what the answers are built from, so the chain
// is walked once end to end rather than assumed (#1831).
func TestADriversRefusalCarriesTheFolder(t *testing.T) {
	home := t.TempDir()
	box := maildir.New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	defer box.Close() //nolint:errcheck
	if err := box.Create("Drafts"); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _, _, err := box.Save("Drafts", fullVolumeReader{}, 0, 0, nil, nil, [16]byte{})
	if err == nil {
		t.Fatal("the save succeeded while the volume refused the body")
	}
	var nospace *mailbox.NoSpaceError
	if !errors.As(err, &nospace) {
		t.Fatalf("the body write was not classified: %v", err)
	}
	if nospace.Folder != "Drafts" {
		t.Errorf("the refusal names folder %q, want Drafts", nospace.Folder)
	}
}

// fullVolumeReader fails the copy the way a full volume fails it.
type fullVolumeReader struct{}

func (fullVolumeReader) Read([]byte) (int, error) { return 0, syscall.ENOSPC }

var _ io.Reader = fullVolumeReader{}
