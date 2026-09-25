package imap

import (
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// virtualConfigured is the driver capability a virtual namespace has: the
// mailbox can say what defines it.
type virtualConfigured interface {
	Config(folder string) (*virtual.Config, error)
}

// syncVirtual brings a virtual mailbox up to date. Membership is decided here,
// not at SEARCH, so EXISTS and FETCH speak of one set (virtual-sync.c:604).
func (s *session) syncVirtual(h *nsHandle, rel string, f *mailbox.Folder) *mailbox.Folder {
	box, ok := mailbox.Driver(h.box).(virtualConfigured)
	if !ok {
		return nil // not a virtual namespace
	}
	cfg, err := box.Config(rel)
	if err != nil {
		slog.Warn("imap: virtual mailbox configuration", "folder", rel, "err", err)
		return nil
	}
	was, _ := virtualHeaderOf(h.idx, f.ID)
	res, err := virtual.Sync(cfg, was, &sessionBacking{s: s})
	if err != nil {
		slog.Warn("imap: virtual sync", "folder", rel, "err", err)
		return nil
	}
	refreshed, err := s.applyVirtual(h, rel, f, res)
	if err != nil {
		slog.Warn("imap: virtual mailbox not updated", "folder", rel, "err", err)
		return nil
	}
	return refreshed
}

// applyVirtual writes what the pass decided; a copy that was here keeps its
// uid, which is stable for the life of a UIDVALIDITY (virtual-sync.c:599).
func (s *session) applyVirtual(h *nsHandle, rel string, f *mailbox.Folder, res virtual.SyncResult) (*mailbox.Folder, error) {
	known := map[[2]uint32]*mailbox.MessageMeta{}
	if !res.Rebuilt {
		was, err := h.idx.GetMessages(f.ID, mailbox.SeqSet{})
		if err != nil {
			return nil, fmt.Errorf("imap/virtual: read records: %w", err)
		}
		for _, m := range was {
			known[[2]uint32{m.VirtualBacking, m.VirtualRealUID}] = m
		}
	}
	records := make([]*mailbox.MessageMeta, 0, len(res.Records))
	next := f.NextUID
	if next == 0 {
		next = 1
	}
	for i := range res.Records {
		rec := res.Records[i]
		if had, ok := known[[2]uint32{rec.VirtualBacking, rec.VirtualRealUID}]; ok {
			rec.UID = had.UID
			rec.ModSeq = had.ModSeq
		} else {
			rec.UID = next
			next++
		}
		records = append(records, &rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].UID < records[j].UID })
	// The header first: it declares the extension, and a record written before
	// that carries no backing folder at all.
	if setter, ok := h.idx.(mailbox.VirtualIndexed); ok {
		if err := setter.SetVirtualHeader(f.ID, res.Header); err != nil {
			return nil, fmt.Errorf("imap/virtual: write header: %w", err)
		}
	}
	if _, err := h.idx.ResetFolder(f.ID, records); err != nil {
		return nil, fmt.Errorf("imap/virtual: write records: %w", err)
	}
	return h.mailbox().Folder(rel, f.UIDValidity)
}

func virtualHeaderOf(idx mailbox.UserIndex, folderID uint64) (mailbox.VirtualHeader, bool) {
	if v, ok := idx.(mailbox.VirtualIndexed); ok {
		return v.VirtualHeader(folderID)
	}
	return mailbox.VirtualHeader{}, false
}

// sessionBacking resolves what a configuration names against the session's own
// personal namespace: the folders a user has are the ones they may draw from.
type sessionBacking struct{ s *session }

func (b *sessionBacking) Folders(cfg *virtual.Config) ([]virtual.Backing, error) {
	h := b.s.primary
	if h == nil {
		return nil, nil
	}
	sep := mailbox.SepOrDefault(string(h.spec.Separator))
	entries, err := h.box.ListFolders()
	if err != nil {
		return nil, fmt.Errorf("imap/virtual: list folders: %w", err)
	}
	var out []virtual.Backing
	for _, e := range entries {
		name := virtual.SameSeparator(e.Name, sep)
		if !virtual.Selects(cfg, name) {
			continue
		}
		f, ferr := mailbox.Counting(h.mailbox()).Folder(e.Name, 0)
		if ferr != nil {
			continue // a folder that cannot be opened contributes nothing
		}
		msgs, merr := h.idx.GetMessages(f.ID, mailbox.SeqSet{})
		if merr != nil {
			return nil, fmt.Errorf("imap/virtual: read %s: %w", e.Name, merr)
		}
		out = append(out, virtual.Backing{
			Name: name, GUID: f.GUID, UIDValidity: f.UIDValidity, Messages: msgs,
		})
	}
	return out, nil
}

// Matches runs the configuration's rule over a backing folder with the same
// matcher a SEARCH uses, so a rule means there what it means on the wire.
func (b *sessionBacking) Matches(back virtual.Backing, rule string) (map[uint32]bool, error) {
	if strings.TrimSpace(rule) == "" {
		return nil, nil // no rule keeps everything
	}
	criteria, err := imapserver.ParseSearchCriteria(rule)
	if err != nil {
		return nil, fmt.Errorf("imap/virtual: rule %q: %w", rule, err)
	}
	// The same question SEARCH asks: a rule about text needs the message.
	needRaw := len(criteria.Header) > 0 || len(criteria.Body) > 0 || len(criteria.Text) > 0 ||
		!criteria.SentSince.IsZero() || !criteria.SentBefore.IsZero() ||
		searchNeedsBodyRecurse(criteria.Not, criteria.Or)
	keep := make(map[uint32]bool, len(back.Messages))
	for i, m := range back.Messages {
		var raw []byte
		if needRaw {
			raw = b.rawOf(back.Name, m)
		}
		flags := make([]imaplib.Flag, 0, len(m.Flags)+len(m.Keywords))
		for _, f := range m.Flags {
			flags = append(flags, imaplib.Flag(f))
		}
		for _, k := range m.Keywords {
			flags = append(flags, imaplib.Flag(k))
		}
		if imapserver.MatchMessage(uint32(i+1), imaplib.UID(m.UID), m.InternalDate,
			int64(m.RFC822Size()), flags, raw, criteria) {
			keep[m.UID] = true
		}
	}
	return keep, nil
}

// rawOf reads a backing message for a rule about its text. Every sync reads
// every body for such a rule; 2b answers those through the index instead.
func (b *sessionBacking) rawOf(folder string, m *mailbox.MessageMeta) []byte {
	h := b.s.primary
	name, err := h.mailbox().MessagePath(folder, m)
	if err != nil {
		return nil
	}
	rc, err := h.box.Fetch(folder, name, m.AltTier)
	if err != nil {
		return nil
	}
	defer rc.Close() //nolint:errcheck
	raw, _ := io.ReadAll(rc)
	return raw
}
