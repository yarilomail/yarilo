package imap

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/pkg/fts"
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
	was := virtualHeaderOf(h.idx, f.ID)
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

func virtualHeaderOf(idx mailbox.UserIndex, folderID uint64) mailbox.VirtualHeader {
	if v, ok := idx.(mailbox.VirtualIndexed); ok {
		hdr, _ := v.VirtualHeader(folderID)
		return hdr
	}
	return mailbox.VirtualHeader{}
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
	// The text part is asked of the index, as SEARCH asks it; only what the
	// index cannot answer, or answers as "maybe", is read.
	textHits, rest, restNeedsBody, verify, indexed := b.textPart(back, criteria)
	keep := make(map[uint32]bool, len(back.Messages))
	for i, m := range back.Messages {
		matchCrit, needRaw := criteria, searchNeedsBody(criteria)
		if indexed {
			if !textHits[m.UID] {
				continue
			}
			if !verify[m.UID] {
				matchCrit, needRaw = rest, restNeedsBody
			}
		}
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
			int64(m.RFC822Size()), flags, raw, matchCrit) {
			keep[m.UID] = true
		}
	}
	return keep, nil
}

// searchNeedsBody is the question SEARCH asks: a criterion about text needs
// the message's bytes.
func searchNeedsBody(c *imaplib.SearchCriteria) bool {
	return len(c.Header) > 0 || len(c.Body) > 0 || len(c.Text) > 0 ||
		!c.SentSince.IsZero() || !c.SentBefore.IsZero() || searchNeedsBodyRecurse(c.Not, c.Or)
}

// textPart asks the index for a rule's text over one backing folder; indexed
// is false when it cannot answer (off, text under NOT/OR, not caught up).
func (b *sessionBacking) textPart(back virtual.Backing, criteria *imaplib.SearchCriteria) (hits map[uint32]bool, rest *imaplib.SearchCriteria, restNeedsBody bool, verify map[uint32]bool, indexed bool) {
	s := b.s
	o := s.srv.opts.FTS
	if !o.enabled() || s.userInfo == nil {
		return nil, nil, false, nil, false
	}
	if len(criteria.Body) == 0 && len(criteria.Text) == 0 && len(criteria.Header) == 0 {
		return nil, nil, false, nil, false
	}
	if searchNeedsBodyRecurse(criteria.Not, criteria.Or) {
		return nil, nil, false, nil, false
	}
	query, stripped, strippedNeedsBody, impossible := s.buildFTSQuery(criteria)
	if impossible {
		return map[uint32]bool{}, stripped, strippedNeedsBody, nil, true
	}
	mbox := fts.MailboxRef{Name: back.Name, GUID: mailbox.FormatObjectID(back.GUID), UIDValidity: back.UIDValidity}
	fallback, imapErr := s.ftsCatchUp(s.userInfo.Username, mbox, back.Messages)
	if imapErr != nil || fallback {
		return nil, nil, false, nil, false
	}
	res, err := o.Client.LookupIn(s.userInfo.Username, []fts.MailboxRef{mbox}, query)
	if err != nil {
		slog.Warn("imap: virtual rule lookup", "folder", back.Name, "err", err)
		return nil, nil, false, nil, false
	}
	hits = make(map[uint32]bool, len(res.Definite)+len(res.Maybe))
	verify = make(map[uint32]bool, len(res.Maybe))
	for _, h := range res.Definite {
		if h.Folder == mbox.GUID {
			hits[h.UID] = true
		}
	}
	for _, h := range res.Maybe {
		if h.Folder == mbox.GUID {
			hits[h.UID] = true
			verify[h.UID] = true
		}
	}
	return hits, stripped, strippedNeedsBody, verify, true
}

// rawOf reads a backing message the index could not answer for: a "maybe"
// hit, text nested under NOT/OR, or a folder not caught up yet.
func (b *sessionBacking) rawOf(folder string, m *mailbox.MessageMeta) []byte {
	rc, err := b.s.primary.mailbox().OpenMessage(folder, m)
	if err != nil {
		return nil
	}
	defer rc.Close() //nolint:errcheck
	raw, _ := io.ReadAll(rc)
	return raw
}

// backingFolder is a folder of the personal namespace a virtual record names.
type backingFolder struct {
	name string
	id   uint64
	guid [16]byte
	uidv uint32
}

// isVirtualSelected reports whether the selected mailbox is a virtual one.
func (s *session) isVirtualSelected() bool {
	if s.folderNS == nil || s.folder == nil {
		return false
	}
	_, ok := mailbox.Driver(s.folderNS.box).(virtualConfigured)
	return ok
}

// backingFolders resolves the selected virtual mailbox's backing ids to the
// folders of the personal namespace, by GUID: a rename keeps the mapping.
func (s *session) backingFolders() (map[uint32]backingFolder, error) {
	if s.backingOf != nil {
		return s.backingOf, nil
	}
	hdr := virtualHeaderOf(s.folderNS.idx, s.folder.ID)
	byGUID := make(map[[16]byte]uint32, len(hdr.Backing))
	for _, b := range hdr.Backing {
		byGUID[b.GUID] = b.ID
	}
	h := s.primary
	entries, err := h.box.ListFolders()
	if err != nil {
		return nil, fmt.Errorf("imap/virtual: list folders: %w", err)
	}
	out := make(map[uint32]backingFolder, len(hdr.Backing))
	for _, e := range entries {
		f, ferr := mailbox.Counting(h.mailbox()).Folder(e.Name, 0)
		if ferr != nil {
			continue
		}
		if id, ok := byGUID[f.GUID]; ok {
			out[id] = backingFolder{name: e.Name, id: f.ID, guid: f.GUID, uidv: f.UIDValidity}
		}
	}
	s.backingOf = out
	return out, nil
}

// readVirtualCopy opens the real message a virtual record names: the backing
// folder by its id, then that folder's own record by the real uid.
func (s *session) readVirtualCopy(m *mailbox.MessageMeta) (io.ReadCloser, error) {
	folders, err := s.backingFolders()
	if err != nil {
		return nil, err
	}
	b, ok := folders[m.VirtualBacking]
	if !ok {
		return nil, fmt.Errorf("imap/virtual: backing folder %d is gone: %w", m.VirtualBacking, os.ErrNotExist)
	}
	h := s.primary
	recs, err := h.idx.GetMessages(b.id, mailbox.SeqSet{{From: m.VirtualRealUID, To: m.VirtualRealUID}})
	if err != nil {
		return nil, fmt.Errorf("imap/virtual: read %s uid %d: %w", b.name, m.VirtualRealUID, err)
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("imap/virtual: %s uid %d is gone: %w", b.name, m.VirtualRealUID, os.ErrNotExist)
	}
	return h.mailbox().OpenMessage(b.name, recs[0])
}

// prepareVirtualFTSSearch catches up each backing folder, then asks once over
// all of them; the virtual mailbox holds nothing to read and is never indexed.
func (s *session) prepareVirtualFTSSearch(criteria *imaplib.SearchCriteria, msgs []*mailbox.MessageMeta) (*ftsFilter, *imaplib.Error) {
	o := s.srv.opts.FTS
	if !o.enabled() || s.userInfo == nil {
		return nil, nil
	}
	if len(criteria.Body) == 0 && len(criteria.Text) == 0 && len(criteria.Header) == 0 {
		return nil, nil
	}
	if searchNeedsBodyRecurse(criteria.Not, criteria.Or) {
		return nil, nil
	}
	query, stripped, strippedNeedsBody, impossible := s.buildFTSQuery(criteria)
	f := &ftsFilter{
		covered:           map[uint32]bool{},
		verify:            map[uint32]bool{},
		stripped:          stripped,
		strippedNeedsBody: strippedNeedsBody,
	}
	if impossible {
		return f, nil
	}
	folders, err := s.backingFolders()
	if err != nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Full-text search unavailable"}
	}
	// Which virtual uid each copy is, and which copies each backing folder has.
	virtualUID := make(map[[2]uint32]uint32, len(msgs))
	perBacking := map[uint32][]uint32{}
	for _, m := range msgs {
		virtualUID[[2]uint32{m.VirtualBacking, m.VirtualRealUID}] = m.UID
		perBacking[m.VirtualBacking] = append(perBacking[m.VirtualBacking], m.UID)
	}
	user := s.userInfo.Username
	var ask []fts.MailboxRef
	idOf := map[string]uint32{}
	for id, b := range folders {
		if len(perBacking[id]) == 0 {
			continue
		}
		mbox := fts.MailboxRef{Name: b.name, GUID: mailbox.FormatObjectID(b.guid), UIDValidity: b.uidv}
		backMsgs, rerr := s.primary.idx.GetMessages(b.id, mailbox.SeqSet{})
		if rerr != nil {
			return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Full-text search unavailable"}
		}
		fallback, imapErr := s.ftsCatchUp(user, mbox, backMsgs)
		if imapErr != nil {
			return nil, imapErr
		}
		if fallback {
			// This folder is not indexed far enough to answer: its copies
			// are read in full rather than answered from what the index has.
			for _, uid := range perBacking[id] {
				f.covered[uid] = true
				f.verify[uid] = true
			}
			continue
		}
		ask = append(ask, mbox)
		idOf[mbox.GUID] = id
	}
	if len(ask) == 0 {
		return f, nil
	}
	res, err := o.Client.LookupIn(user, ask, query)
	if err != nil {
		slog.Warn("imap: fts lookup over a virtual mailbox failed", "user", user, "err", err)
		if o.ReadFallback {
			return nil, nil
		}
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Full-text search unavailable"}
	}
	take := func(hits []fts.FolderHit, verify bool) {
		for _, hit := range hits {
			uid, ok := virtualUID[[2]uint32{idOf[hit.Folder], hit.UID}]
			if !ok {
				continue // a copy the rule kept out of this mailbox
			}
			f.covered[uid] = true
			if verify || o.Strict {
				f.verify[uid] = true
			}
		}
	}
	take(res.Definite, false)
	take(res.Maybe, true)
	return f, nil
}
