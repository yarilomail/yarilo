package imap

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/internal/storage/search"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// virtualConfigured is the driver capability a virtual namespace has: the
// mailbox can say what defines it.
type virtualConfigured interface {
	Config(folder string) (*virtual.Config, error)
}

// virtualSyncFailed logs why a pass failed and answers the client the way a
// storage failure is answered: the records stay as the last pass left them.
func (s *session) virtualSyncFailed(rel string, err error) *imaplib.Error {
	slog.Error("imap: virtual mailbox not updated", "user", s.username(), "folder", rel, "err", err)
	return &imaplib.Error{
		Type: imaplib.StatusResponseTypeNo,
		Code: imaplib.ResponseCodeServerBug,
		Text: "Internal error occurred. Refer to server log for more information.",
	}
}

// writeSyncFailure reports a failed pass untagged, so the command still completes.
func writeSyncFailure(w *imapserver.UpdateWriter, err *imaplib.Error) error {
	return w.WriteStatusResp((*imaplib.StatusResponse)(err))
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
	out, err := s.backingFoldersOf(s.folderNS, s.folder.ID)
	if err != nil {
		return nil, err
	}
	s.backingOf = out
	return out, nil
}

// backingFoldersOf is backingFolders for any virtual mailbox: the store says
// where its copies live, and the personal namespace opens them.
func (s *session) backingFoldersOf(h *nsHandle, folderID uint64) (map[uint32]backingFolder, error) {
	vc, ok := h.mailbox().(mailbox.VirtualCopies)
	if !ok {
		return nil, fmt.Errorf("imap/virtual: %s holds no copies", h.name)
	}
	refs, err := vc.Backing(folderID)
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]backingFolder, len(refs))
	for id, ref := range refs {
		f, ferr := mailbox.Counting(s.primary.mailbox()).Folder(ref.Name, 0)
		if ferr != nil {
			continue
		}
		out[id] = backingFolder{name: ref.Name, id: f.ID, guid: f.GUID, uidv: f.UIDValidity, folder: f}
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
	// As it is, then a poll pass: a mailbox never selected still knows its
	// folders, and a failed pass keeps the ones the last saw.
	f, err := mailbox.Counting(h.mailbox()).Folder(rel, 0)
	if err != nil {
		return nil
	}
	if refreshed, _ := h.mailbox().Poll(f); refreshed != nil {
		f = refreshed
	}
	folders, err := s.backingFoldersOf(h, f.ID)
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
	recs, err := h.mailbox().Messages(b.id, mailbox.SeqSet{{From: m.VirtualRealUID, To: m.VirtualRealUID}})
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
func (s *session) prepareVirtualFTSSearch(criteria *imaplib.SearchCriteria, msgs []*mailbox.MessageMeta) (*search.Plan, error) {
	o := s.srv.opts.FTS.searchOptions()
	if !o.Indexed() || s.userInfo == nil || !search.TextOnTop(criteria) {
		return nil, nil
	}
	query, stripped, strippedNeedsBody, impossible := o.Query(criteria)
	f := &search.Plan{Covered: map[uint32]bool{}, Verify: map[uint32]bool{}, Rest: stripped, RestNeedsBody: strippedNeedsBody}
	if impossible {
		return f, nil
	}
	folders, err := s.backingFolders()
	if err != nil {
		return nil, search.ErrLookup
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
		backMsgs, rerr := s.primary.mailbox().Messages(b.id, mailbox.SeqSet{})
		if rerr != nil {
			return nil, search.ErrLookup
		}
		fallback, cerr := o.CatchUp(user, mbox, backMsgs)
		if cerr != nil {
			return nil, cerr
		}
		if fallback {
			// This folder is not indexed far enough to answer: its copies
			// are read in full rather than answered from what the index has.
			for _, uid := range perBacking[id] {
				f.Covered[uid] = true
				f.Verify[uid] = true
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
		return nil, fmt.Errorf("%w: %w", search.ErrLookup, err)
	}
	take := func(hits []fts.FolderHit, verify bool) {
		for _, hit := range hits {
			uid, ok := virtualUID[[2]uint32{idOf[hit.Folder], hit.UID}]
			if !ok {
				continue // a copy the rule kept out of this mailbox
			}
			f.Covered[uid] = true
			if verify || o.Strict {
				f.Verify[uid] = true
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
		tx, terr := h.mailbox().Begin(b.id)
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
		reals, rerr := h.mailbox().Messages(b.id, mailbox.SeqSet{})
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
			recs, rerr := readForWriteSet(h.mailbox(), b.id, mailbox.SeqSet{{From: vm.VirtualRealUID, To: vm.VirtualRealUID}})
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
	tx, terr := s.folderMailbox().Begin(s.folder.ID)
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
			recs, err := h.mailbox().Messages(b.id, mailbox.SeqSet{{From: c.uid, To: c.uid}})
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
// on; a virtual one also hears every folder it draws from (as the reference does).
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

// errVirtualCannot is the refusal a virtual mailbox gives to what only its
// configuration file may do, or what it cannot hold.
func errVirtualCannot(text string) error {
	return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCodeCannot, Text: text}
}

// isVirtualName reports whether name is a mailbox of a virtual namespace.
func (s *session) isVirtualName(name string) bool {
	h, _, err := s.dispatch(name)
	if err != nil || h == nil {
		return false
	}
	_, ok := mailbox.Driver(h.box).(virtualConfigured)
	return ok
}

// virtualSaveTarget is where writes into name store: name, or a virtual
// mailbox's "!" folder, then answered without UIDs (as the reference does).
func (s *session) virtualSaveTarget(name string) (target string, redirected bool, err error) {
	h, rel, derr := s.dispatch(name)
	if derr != nil || h == nil {
		return name, false, nil
	}
	box, ok := mailbox.Driver(h.box).(virtualConfigured)
	if !ok {
		return name, false, nil
	}
	cfg, cerr := box.Config(rel)
	if cerr != nil {
		return "", false, cerr
	}
	if cfg.SaveTo == nil || s.primary == nil {
		return "", false, errVirtualCannot("Can't save messages to this virtual mailbox")
	}
	target = s.primary.fullName(cfg.SaveTo.Pattern)
	if exists, _ := s.primary.box.FolderExists(cfg.SaveTo.Pattern); !exists {
		return "", false, errVirtualCannot("the folder this virtual mailbox saves to does not exist: " + target)
	}
	return target, true, nil
}

// pollVirtual runs the selected virtual mailbox's poll pass, as the reference
// follows the folders; a failure goes out untagged and the command completes.
func (s *session) pollVirtual(w *imapserver.UpdateWriter) error {
	if !s.isVirtualSelected() {
		return nil
	}
	refreshed, err := s.folderMailbox().Poll(s.folder)
	if err != nil {
		return writeSyncFailure(w, s.virtualSyncFailed(s.folder.Name, err))
	}
	if refreshed != nil {
		s.backingOf = nil // the header may name folders it did not
		s.virtualMoved = true
	}
	return nil
}

// reopenSelected reopens the selected folder; a virtual one as it is, its poll
// pass having run, so a NOOP drops no record that stopped matching.
func (s *session) reopenSelected() (*mailbox.Folder, error) {
	if s.isVirtualSelected() {
		return mailbox.Counting(s.folderMailbox()).Folder(s.folder.Name, s.folder.UIDValidity)
	}
	return s.folderMailbox().Folder(s.folder.Name, s.folder.UIDValidity)
}
