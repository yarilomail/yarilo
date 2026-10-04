package search

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// fakeIndex answers lookups from fixed hits; the embedded interface is nil, so
// any other call panics.
type fakeIndex struct {
	ftsproto.Client
	definite, maybe []uint32
	err             error
	indexed         uint32
}

func (f *fakeIndex) Status(string, fts.MailboxRef) (uint32, uint32, error) { return f.indexed, 0, nil }
func (f *fakeIndex) Prepend(string, fts.MailboxRef, uint32) error          { return nil }
func (f *fakeIndex) Lookup(string, fts.MailboxRef, fts.Query) (fts.Result, error) {
	return fts.Result{Definite: f.definite, Maybe: f.maybe}, f.err
}
func (f *fakeIndex) LookupIn(_ string, ms []fts.MailboxRef, _ fts.Query) (fts.SetResult, error) {
	var r fts.SetResult
	for _, u := range f.definite {
		r.Definite = append(r.Definite, fts.FolderHit{Folder: ms[0].GUID, UID: u})
	}
	for _, u := range f.maybe {
		r.Maybe = append(r.Maybe, fts.FolderHit{Folder: ms[0].GUID, UID: u})
	}
	return r, f.err
}

// bodies serves message bytes by uid and counts what was opened.
type bodies struct {
	mailbox.Box
	text   map[uint32]string
	opened int
}

func (b *bodies) OpenMessage(_ string, m *mailbox.MessageMeta) (io.ReadCloser, error) {
	b.opened++
	return io.NopCloser(strings.NewReader(b.text[m.UID])), nil
}

func folderOf(t *testing.T) (*mailbox.Folder, []*mailbox.MessageMeta, *bodies) {
	t.Helper()
	f := &mailbox.Folder{Name: "INBOX", GUID: [16]byte{1}, UIDValidity: 7}
	text := map[uint32]string{
		1: "Subject: needle one\r\n\r\nbody\r\n",
		2: "Subject: other\r\n\r\nbody\r\n",
		3: "Subject: needle three\r\n\r\nbody\r\n",
	}
	var msgs []*mailbox.MessageMeta
	for uid := uint32(1); uid <= 3; uid++ {
		msgs = append(msgs, &mailbox.MessageMeta{UID: uid, Size: uint32(len(text[uid])), InternalDate: time.Unix(1700000000, 0)})
	}
	return f, msgs, &bodies{text: text}
}

func chain(t *testing.T) *language.MultiChain {
	t.Helper()
	c, err := language.NewMultiChain([]string{"english"}, nil, nil, language.DefaultTokenMaxLen, language.DefaultAddressMaxLen, 10)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A rule over a folder keeps what it matches: by the index where it answers,
// reading only what it calls maybe; by the scan where it cannot answer.
func TestAFolderRuleKeepsWhatItMatches(t *testing.T) {
	for _, tc := range []struct {
		name       string
		index      *fakeIndex
		wantKeep   string
		wantOpened int
	}{
		{"no index: the scan reads every body", nil, "[1 3]", 3},
		{"the index answers: definite read nothing, maybe read once", &fakeIndex{definite: []uint32{1}, maybe: []uint32{3}}, "[1 3]", 1},
		{"a definite hit the index got wrong is kept, as the index said", &fakeIndex{definite: []uint32{2}}, "[2]", 0},
		{"the index fails: the scan stands in", &fakeIndex{err: errors.New("down")}, "[1 3]", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, msgs, box := folderOf(t)
			o := Options{}
			if tc.index != nil {
				tc.index.indexed = 3
				o = Options{Client: tc.index, Chain: chain(t), Enabled: true}
			}
			keep, err := o.Folder(box, "u", f, msgs, "SUBJECT needle")
			if err != nil {
				t.Fatal(err)
			}
			var got []uint32
			for uid := uint32(1); uid <= 3; uid++ {
				if keep[uid] {
					got = append(got, uid)
				}
			}
			if s := fmt.Sprint(got); s != tc.wantKeep {
				t.Errorf("kept %s, want %s", s, tc.wantKeep)
			}
			if box.opened != tc.wantOpened {
				t.Errorf("opened %d bodies, want %d", box.opened, tc.wantOpened)
			}
		})
	}
}

// A SEARCH the index cannot serve says why: an outage is retried, a mailbox
// still indexing is retried, a fallback scans instead.
func TestAPlanSaysWhyTheIndexCouldNotAnswer(t *testing.T) {
	c := &imaplib.SearchCriteria{Body: []string{"needle"}}
	_, msgs, _ := folderOf(t)
	ref := RefOf(&mailbox.Folder{Name: "INBOX", GUID: [16]byte{1}})
	t.Run("an outage carries the service's error", func(t *testing.T) {
		o := Options{Client: &fakeIndex{indexed: 3, err: ftsproto.ErrUnavailable}, Chain: chain(t), Enabled: true}
		if _, err := o.Plan("u", ref, c, msgs); !errors.Is(err, ftsproto.ErrUnavailable) {
			t.Errorf("plan answered %v, want the outage", err)
		}
	})
	t.Run("with a fallback the outage scans", func(t *testing.T) {
		o := Options{Client: &fakeIndex{indexed: 3, err: ftsproto.ErrUnavailable}, Chain: chain(t), Enabled: true, ReadFallback: true}
		if p, err := o.Plan("u", ref, c, msgs); p != nil || err != nil {
			t.Errorf("plan answered %v, %v; want a scan", p, err)
		}
	})
	t.Run("an index behind with no time to wait is still indexing", func(t *testing.T) {
		o := Options{Client: &fakeIndex{indexed: 1}, Chain: chain(t), Enabled: true, AddMissing: "priority", Timeout: time.Nanosecond}
		if _, err := o.Plan("u", ref, c, msgs); !errors.Is(err, ErrStillIndexing) {
			t.Errorf("plan answered %v, want still indexing", err)
		}
	})
	t.Run("text under NOT beside text on top is left to the scan", func(t *testing.T) {
		o := Options{Client: &fakeIndex{indexed: 3}, Chain: chain(t), Enabled: true}
		nested := &imaplib.SearchCriteria{Body: []string{"needle"}, Not: []imaplib.SearchCriteria{{Body: []string{"other"}}}}
		if p, err := o.Plan("u", ref, nested, msgs); p != nil || err != nil {
			t.Errorf("plan answered %v, %v; want a scan", p, err)
		}
	})
}
