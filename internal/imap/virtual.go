package imap

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// virtualConfigured is the driver capability a virtual namespace has: the
// mailbox can say what defines it.
type virtualConfigured interface {
	Config(folder string) (*virtual.Config, error)
}

// syncVirtual brings a virtual mailbox up to date. Membership is decided here,
// not at SEARCH, so EXISTS and FETCH speak of one set (virtual-sync.c:604).
func (s *session) syncVirtual(h *nsHandle, rel string, f *mailbox.Folder, mode virtual.Mode) *mailbox.Folder {
	box, ok := mailbox.Driver(h.box).(virtualConfigured)
	if !ok {
		return nil // not a virtual namespace
	}
	cfg, err := box.Config(rel)
	if err != nil {
		slog.Warn("imap: virtual mailbox configuration", "folder", rel, "err", err)
		return nil
	}
	// The check reads only the folders' state, so a mailbox where nothing
	// moved costs no hold; the pass checks again under it.
	moved, err := virtual.Moved(cfg, virtualHeaderOf(h.idx, f.ID), &sessionBacking{s: s})
	if err != nil {
		slog.Warn("imap: virtual mailbox check", "folder", rel, "err", err)
		return nil
	}
	if !moved {
		return nil
	}
	var refreshed *mailbox.Folder
	pass := func(context.Context) error {
		// Read under the hold: two sessions deciding from one stale view
		// would give one copy two uids.
		was := virtualHeaderOf(h.idx, f.ID)
		old, rerr := h.idx.GetMessages(f.ID, mailbox.SeqSet{})
		if rerr != nil {
			return fmt.Errorf("imap/virtual: read records: %w", rerr)
		}
		res, serr := virtual.Sync(cfg, was, old, mode, &sessionBacking{s: s})
		if serr != nil {
			return serr
		}
		if !res.Changed {
			return nil
		}
		refreshed, serr = s.applyVirtual(h, rel, f, res, old)
		return serr
	}
	if l := s.srv.opts.Locker; l != nil {
		ctx := locks.WithSite(context.Background(), "virtual-sync")
		err = locks.WithLockWaiting(ctx, l, locks.VirtualSyncKey(s.userInfo.Username, rel),
			locks.Owner(s.userInfo.Username, s.sid), 30*time.Second, 10*time.Second, 10*time.Second, pass)
	} else {
		err = pass(context.Background())
	}
	if err != nil {
		slog.Warn("imap: virtual mailbox not updated", "folder", rel, "err", err)
		return nil
	}
	if refreshed != nil && s.folder != nil && h == s.folderNS && f.ID == s.folder.ID {
		s.backingOf = nil // the header may name folders it did not
		s.virtualMoved = true
	}
	return refreshed
}

// applyVirtual writes what the pass decided as ordinary index changes: a record
// that left is expunged, so its modseq and VANISHED reach QRESYNC (RFC 7162).
func (s *session) applyVirtual(h *nsHandle, rel string, f *mailbox.Folder, res virtual.SyncResult, old []*mailbox.MessageMeta) (*mailbox.Folder, error) {
	// The header first: it declares the extension, and a record written before
	// that carries no backing folder at all.
	if setter, ok := h.idx.(mailbox.VirtualIndexed); ok {
		if err := setter.SetVirtualHeader(f.ID, res.Header); err != nil {
			return nil, fmt.Errorf("imap/virtual: write header: %w", err)
		}
	}
	tx, err := h.idx.Begin(f.ID)
	if err != nil {
		return nil, fmt.Errorf("imap/virtual: open records: %w", err)
	}
	defer tx.Rollback()
	stays := make(map[uint32]bool, len(res.Records))
	for i := range res.Records {
		rec := res.Records[i]
		switch {
		case rec.UID == 0:
			tx.Append(&rec) // a new copy: the index gives it the next uid
		case rec.ModSeq == 0:
			stays[rec.UID] = true
			tx.UpdateFlags(rec.UID, mailbox.FlagsUpdate{Flags: rec.Flags, Keywords: rec.Keywords})
		default:
			stays[rec.UID] = true
		}
	}
	for _, m := range old {
		if !stays[m.UID] {
			tx.Expunge(m.UID)
		}
	}
	if _, err := tx.Commit(); err != nil {
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
type sessionBacking struct {
	s   *session
	ids map[[16]byte]uint64 // folder id by GUID, from Folders
}

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
		if b.ids == nil {
			b.ids = map[[16]byte]uint64{}
		}
		b.ids[f.GUID] = f.ID
		out = append(out, virtual.Backing{
			Name: name, GUID: f.GUID, UIDValidity: f.UIDValidity,
			NextUID: f.NextUID, HighestModSeq: f.HighestModSeq,
		})
	}
	return out, nil
}

func (b *sessionBacking) Messages(back virtual.Backing) ([]*mailbox.MessageMeta, error) {
	id, ok := b.ids[back.GUID]
	if !ok {
		return nil, fmt.Errorf("imap/virtual: %s was not listed", back.Name)
	}
	msgs, err := b.s.primary.idx.GetMessages(id, mailbox.SeqSet{})
	if err != nil {
		return nil, fmt.Errorf("imap/virtual: read %s: %w", back.Name, err)
	}
	return msgs, nil
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
	name   string
	id     uint64
	guid   [16]byte
	uidv   uint32
	folder *mailbox.Folder
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
	out, err := s.backingFoldersOf(s.folderNS.idx, s.folder.ID)
	if err != nil {
		return nil, err
	}
	s.backingOf = out
	return out, nil
}

// backingFoldersOf is backingFolders for any virtual mailbox, selected or not.
func (s *session) backingFoldersOf(idx mailbox.UserIndex, folderID uint64) (map[uint32]backingFolder, error) {
	hdr := virtualHeaderOf(idx, folderID)
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
			out[id] = backingFolder{name: e.Name, id: f.ID, guid: f.GUID, uidv: f.UIDValidity, folder: f}
		}
	}
	return out, nil
}

// virtualBackingNames names the folders a virtual mailbox draws from, synced
// first so a mailbox never selected still knows them; nil for any other.
func (s *session) virtualBackingNames(name string) []string {
	h, rel, err := s.dispatch(name)
	if err != nil || h == nil {
		return nil
	}
	if _, ok := mailbox.Driver(h.box).(virtualConfigured); !ok {
		return nil
	}
	f, err := h.mailbox().Folder(rel, 0)
	if err != nil {
		return nil
	}
	if refreshed := s.syncVirtual(h, rel, f, virtual.Poll); refreshed != nil {
		f = refreshed
	}
	folders, err := s.backingFoldersOf(h.idx, f.ID)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(folders))
	for _, b := range folders {
		names = append(names, b.name)
	}
	return names
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

// virtualCopy is where the message a virtual record names is kept; its file
// name is read when needed, since a flag write renames it.
type virtualCopy struct {
	back backingFolder
	uid  uint32
}

// storeOnCopies applies a STORE to the copies the virtual records name and
// answers with the set each ended with; a record whose copy is gone drops out.
func (s *session) storeOnCopies(msgs map[uint32]*mailbox.MessageMeta, updates map[uint32]mailbox.FlagsUpdate) (map[uint32]mailbox.FlagsUpdate, map[uint32]virtualCopy, error) {
	folders, err := s.backingFolders()
	if err != nil {
		return nil, nil, err
	}
	byBacking := map[uint32][]uint32{}
	for vuid := range updates {
		if m := msgs[vuid]; m != nil && m.VirtualBacking != 0 {
			byBacking[m.VirtualBacking] = append(byBacking[m.VirtualBacking], vuid)
		}
	}
	h := s.primary
	out := make(map[uint32]mailbox.FlagsUpdate, len(updates))
	copies := make(map[uint32]virtualCopy, len(updates))
	for id, vuids := range byBacking {
		b, ok := folders[id]
		if !ok {
			continue // the backing folder is gone: nothing to change
		}
		tx, terr := h.idx.Begin(b.id)
		if terr != nil {
			return nil, nil, terr
		}
		for _, vuid := range vuids {
			tx.UpdateFlags(msgs[vuid].VirtualRealUID, updates[vuid])
		}
		res, cerr := tx.Commit()
		tx.Rollback()
		if cerr != nil {
			return nil, nil, cerr
		}
		reals, rerr := h.idx.GetMessages(b.id, mailbox.SeqSet{})
		if rerr != nil {
			return nil, nil, rerr
		}
		realOf := make(map[uint32]*mailbox.MessageMeta, len(reals))
		for _, r := range reals {
			realOf[r.UID] = r
		}
		var writes []mailbox.FlagWrite
		for _, vuid := range vuids {
			real := msgs[vuid].VirtualRealUID
			r, ok := res.Flags[real]
			if !ok {
				continue // the copy was expunged meanwhile
			}
			out[vuid] = mailbox.FlagsUpdate{Flags: r.Flags, Keywords: r.Keywords}
			if name, nerr := h.mailbox().MessagePath(b.name, realOf[real]); nerr == nil {
				writes = append(writes, mailbox.FlagWrite{UID: real, Filename: name, Flags: r.Flags, Keywords: r.Keywords})
				copies[vuid] = virtualCopy{back: b, uid: real}
			}
		}
		h.mailbox().WriteFlags(b.folder, b.name, writes)
		s.emitMailboxChange(b.folder, locks.EventChanged, 0)
	}
	return out, copies, nil
}

// expungeCopies removes the real messages the doomed virtual records name, then
// the records whose copy is gone; a copy that stays keeps its record and uid.
func (s *session) expungeCopies(doomed []*mailbox.MessageMeta) ([]*mailbox.MessageMeta, error) {
	folders, err := s.backingFolders()
	if err != nil {
		return nil, err
	}
	h := s.primary
	byBacking := map[uint32][]*mailbox.MessageMeta{}
	for _, m := range doomed {
		byBacking[m.VirtualBacking] = append(byBacking[m.VirtualBacking], m)
	}
	var gone []*mailbox.MessageMeta
	for id, vms := range byBacking {
		b, ok := folders[id]
		if !ok {
			gone = append(gone, vms...) // the backing folder itself is gone
			continue
		}
		var reals []*mailbox.MessageMeta
		for _, vm := range vms {
			recs, rerr := h.idx.GetMessages(b.id, mailbox.SeqSet{{From: vm.VirtualRealUID, To: vm.VirtualRealUID}})
			if rerr != nil {
				return nil, fmt.Errorf("imap/virtual: read %s uid %d: %w", b.name, vm.VirtualRealUID, rerr)
			}
			if len(recs) == 0 {
				gone = append(gone, vm)
				continue
			}
			reals = append(reals, recs...)
		}
		removed, _, herr := h.mailbox().ExpungeMarked(b.folder, b.name, reals)
		if herr != nil {
			return nil, herr
		}
		out := make(map[uint32]bool, len(removed))
		for _, r := range removed {
			out[r.UID] = true
			s.emitMailboxChangeSized(b.folder, locks.EventExpunged, r.UID, usageDelta(r), r.GUID)
		}
		for _, vm := range vms {
			if out[vm.VirtualRealUID] {
				gone = append(gone, vm)
			}
		}
	}
	if len(gone) == 0 {
		return nil, nil
	}
	tx, terr := s.folderIdx().Begin(s.folder.ID)
	if terr != nil {
		return nil, terr
	}
	defer tx.Rollback()
	for _, m := range gone {
		tx.Expunge(m.UID)
	}
	if _, cerr := tx.Commit(); cerr != nil {
		return nil, cerr
	}
	return gone, nil
}

// imapSieveOnCopies runs the FLAG cause as the reference does for a STORE in a
// virtual mailbox: the copy's folder first, then the virtual mailbox's script.
func (s *session) imapSieveOnCopies(pending []pendingStore, copies map[uint32]virtualCopy, changed []string) {
	h := s.primary
	virtScript := s.imapSieveScriptName(s.folderNS, s.folder.Name, s.folder.GUID)
	virtName := s.folderNS.fullName(s.folder.Name)
	backScript := map[uint64]string{}
	for _, p := range pending {
		c, ok := copies[p.uid]
		if !ok {
			continue
		}
		b := c.back
		script, known := backScript[b.id]
		if !known {
			script = s.imapSieveScriptName(h, b.name, b.guid)
			backScript[b.id] = script
		}
		for _, run := range []struct{ script, mailbox string }{{script, b.name}, {virtScript, virtName}} {
			// Read afresh each time: the flag write and the first script may
			// have renamed the copy's file or moved the copy away.
			recs, err := h.idx.GetMessages(b.id, mailbox.SeqSet{{From: c.uid, To: c.uid}})
			if err != nil || len(recs) == 0 {
				break
			}
			name, nerr := h.mailbox().MessagePath(b.name, recs[0])
			if nerr != nil {
				break
			}
			s.runImapSieveScript(run.script, "FLAG", run.mailbox, b.name, h, b.folder, c.uid, name, recs[0].AltTier, "", changed)
		}
	}
}

// subscribeSelected is the event stream an IDLE on the selected mailbox waits
// on; a virtual one also hears every folder it draws from (virtual-storage.c:693).
func (s *session) subscribeSelected(ctx context.Context) (<-chan locks.Event, error) {
	l, user := s.srv.opts.Locker, s.userInfo.Username
	keys := []string{locks.MailboxKey(user, s.folder.Name)}
	if s.isVirtualSelected() {
		if folders, err := s.backingFolders(); err == nil {
			for _, b := range folders {
				keys = append(keys, locks.MailboxKey(user, b.name))
			}
		}
	}
	return subscribeAll(ctx, l, keys)
}

// subscribeAll merges the event streams of several keys into one, closed when
// every one of them is.
func subscribeAll(ctx context.Context, l locks.Locker, keys []string) (<-chan locks.Event, error) {
	if len(keys) == 1 {
		return l.Subscribe(ctx, keys[0])
	}
	out := make(chan locks.Event, 16)
	var wg sync.WaitGroup
	for _, k := range keys {
		ch, err := l.Subscribe(ctx, k)
		if err != nil {
			return nil, fmt.Errorf("imap/virtual: subscribe %s: %w", k, err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ev := range ch {
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(out) }()
	return out, nil
}
