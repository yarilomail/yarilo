//go:build flatcurve

package ftsservice

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/fts/flatcurve"
	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// newSdboxTestService is newTestServiceIn on sdbox, the driver that heals and
// whose expunge removes the message file, as on the stand (#2257).
func newSdboxTestService(t *testing.T) (*Service, mailbox.UserMailbox, mailbox.UserIndex, string) {
	t.Helper()
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	mb := dboxv2.New()
	idx := file.New()
	set := language.DefaultSettings()
	chain, err := language.NewMultiChain([]string{set.Language}, set.Filters, nil, set.TokenMaxLen, set.AddressMaxLen, 0)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(testOpts(Options{
		Engine:      flatcurve.New(flatcurve.Options{CommitLimit: 500, MinTermSize: 2, RotateCount: 5000}),
		Mailbox:     mb,
		Index:       idx,
		ResolveUser: func(u string) (*mailbox.UserInfo, error) { return resolver.UserInfo(u, "") },
		Chain:       chain,
		CommitLimit: 2,
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() }) //nolint:errcheck
	info, _ := resolver.UserInfo(testUser, "")
	box := mb.OpenUser(info)
	uidx := idx.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { box.Close(); uidx.Close() }) //nolint:errcheck
	return svc, box, uidx, info.Home
}

// A message expunged after the job listed it reads as a missing file: that is
// an expunge, and the folder is not flagged for a heal; a missing file whose
// record is still there is corruption and still is (#2257).
func TestAReadOfAMessageExpungedMidJobFlagsNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expunge  bool
		wantMark bool
	}{
		{"expunged: record and file gone", true, false},
		{"file gone, record still there", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, box, uidx, home := newSdboxTestService(t)
			saveMessage(t, box, uidx, 1, "alpha")
			saveMessage(t, box, uidx, 2, "bravo")
			svc.opts.afterList = func() {
				if tc.expunge {
					expungeCopy(t, uidx, testMbox.Name, 1)
				}
				matches, _ := filepath.Glob(filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails", "u.1"))
				if len(matches) != 1 {
					t.Errorf("message file of uid 1 not found under %s", home)
					return
				}
				if err := os.Remove(matches[0]); err != nil {
					t.Error(err)
				}
			}
			if err := svc.Index(testUser, testMbox, 2, 0); err != nil {
				t.Fatalf("index: %v", err)
			}
			waitIndexedIn(t, svc, testMbox, 2)
			f, err := uidx.OpenFolder(testMbox.Name, testMbox.UIDValidity)
			if err != nil {
				t.Fatal(err)
			}
			if f.Fsckd != tc.wantMark {
				t.Errorf("folder flagged for a heal = %v, want %v", f.Fsckd, tc.wantMark)
			}
		})
	}
}
