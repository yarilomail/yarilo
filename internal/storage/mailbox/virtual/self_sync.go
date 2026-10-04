package virtual

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/search"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Options is what a virtual namespace draws on: the user's personal mail, the
// search evaluator, the annotations a rule may test, and the sync lock.
type Options struct {
	// Personal opens a user's personal namespace, as its assembler builds it.
	Personal     func(*mailbox.UserInfo) mailbox.Box
	Search       search.Options
	MetadataDict dict.Dict
	Locker       locks.Locker
}

// errNoPersonal: the namespace was built without the mail it draws from.
var errNoPersonal = errors.New("virtual: no personal namespace to draw from")

// personal is the mail this user's virtual mailboxes draw from, opened on the
// first pass; the base rereads the index on each look, so it never goes stale.
func (u *userMailbox) personal() (mailbox.Box, error) {
	if u.opts.Personal == nil || u.info.Personal == nil {
		return nil, errNoPersonal
	}
	u.personalMu.Lock()
	defer u.personalMu.Unlock()
	if u.personalBox == nil {
		u.personalBox = u.opts.Personal(u.info.Personal)
	}
	return u.personalBox, nil
}

// SyncFolder brings one virtual mailbox up to date with the folders it draws
// from, under a lock per mailbox; open drops a record that stopped matching.
func (u *userMailbox) SyncFolder(idx mailbox.UserIndex, f *mailbox.Folder, open bool) (bool, error) {
	cfg, err := u.Config(f.Name)
	if err != nil {
		return false, err
	}
	box, err := u.personal()
	if err != nil {
		return false, err
	}
	r := &resolver{u: u, box: box}
	was := headerOf(idx, f.ID)
	if adopted, took := u.adoptNames(idx, f, cfg, was, r); took {
		was = adopted
	}
	// Reads only the folders' state, so a mailbox where nothing moved costs no
	// hold; the pass checks again under it.
	moved, err := Moved(cfg, was, r)
	if err != nil || !moved {
		return false, err
	}
	mode := Poll
	if open {
		mode = Open
	}
	changed := false
	pass := func(context.Context) error {
		// Under the hold: two passes deciding from one stale view would give
		// one copy two uids.
		was := headerOf(idx, f.ID)
		old, rerr := idx.GetMessages(f.ID, mailbox.SeqSet{})
		if rerr != nil {
			return fmt.Errorf("virtual: read records: %w", rerr)
		}
		res, serr := Sync(cfg, was, old, mode, r)
		if serr != nil || !res.Changed {
			return serr
		}
		changed = true
		return apply(idx, f, res, old)
	}
	if l := u.opts.Locker; l != nil {
		ctx := locks.WithSite(context.Background(), "virtual-sync")
		err = locks.WithLockWaiting(ctx, l, locks.VirtualSyncKey(u.info.Username, f.Name),
			locks.Owner(u.info.Username, u.info.SessionID), 30*time.Second, 10*time.Second, 10*time.Second, pass)
	} else {
		err = pass(context.Background())
	}
	return changed, err
}

// adoptNames gives the folders of a header another implementation wrote the
// identity kept here, so a migrated mailbox keeps its uids.
func (u *userMailbox) adoptNames(idx mailbox.UserIndex, f *mailbox.Folder, cfg *Config, was mailbox.VirtualHeader, r Resolver) (mailbox.VirtualHeader, bool) {
	if !NeedsNames(was) {
		return was, false
	}
	folders, err := r.Folders(cfg)
	if err != nil {
		return was, false
	}
	adopted, took := AdoptNames(was, folders)
	setter, ok := idx.(mailbox.VirtualIndexed)
	if !took || !ok {
		return was, false
	}
	if err := setter.SetVirtualHeader(f.ID, adopted); err != nil {
		slog.Warn("virtual: mailbox keeps the names it was given", "folder", f.Name, "err", err)
		return was, false
	}
	slog.Info("virtual: mailbox taken over from another index", "user", u.info.Username, "folder", f.Name, "folders", len(adopted.Backing))
	return adopted, true
}

// apply writes what a pass decided as ordinary index changes: a record that
// left is expunged, so its modseq and VANISHED reach QRESYNC (RFC 7162).
func apply(idx mailbox.UserIndex, f *mailbox.Folder, res SyncResult, old []*mailbox.MessageMeta) error {
	// The header first: it declares the extension a record's backing needs.
	if setter, ok := idx.(mailbox.VirtualIndexed); ok {
		if err := setter.SetVirtualHeader(f.ID, res.Header); err != nil {
			return fmt.Errorf("virtual: write header: %w", err)
		}
	}
	tx, err := idx.Begin(f.ID)
	if err != nil {
		return fmt.Errorf("virtual: open records: %w", err)
	}
	defer tx.Rollback()
	stays := make(map[uint32]bool, len(res.Records))
	for i := range res.Records {
		rec := res.Records[i]
		switch {
		case rec.UID == 0:
			tx.Append(&rec)
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
		return fmt.Errorf("virtual: write records: %w", err)
	}
	return nil
}

func headerOf(idx mailbox.UserIndex, folderID uint64) mailbox.VirtualHeader {
	if v, ok := idx.(mailbox.VirtualIndexed); ok {
		hdr, _ := v.VirtualHeader(folderID)
		return hdr
	}
	return mailbox.VirtualHeader{}
}

// ResolveBacking names, by the id each record carries, the personal folder a
// virtual mailbox's copies live in, by GUID so a rename keeps the mapping.
func (u *userMailbox) ResolveBacking(idx mailbox.UserIndex, folderID uint64) (map[uint32]mailbox.BackingRef, error) {
	box, err := u.personal()
	if err != nil {
		return nil, err
	}
	hdr := headerOf(idx, folderID)
	byGUID := make(map[[16]byte]uint32, len(hdr.Backing))
	for _, b := range hdr.Backing {
		byGUID[b.GUID] = b.ID
	}
	entries, err := box.Store().ListFolders()
	if err != nil {
		return nil, fmt.Errorf("virtual: list folders: %w", err)
	}
	out := make(map[uint32]mailbox.BackingRef, len(hdr.Backing))
	for _, e := range entries {
		f, ferr := mailbox.Counting(box).Folder(e.Name, 0)
		if ferr != nil {
			continue
		}
		if id, ok := byGUID[f.GUID]; ok {
			out[id] = mailbox.BackingRef{Name: e.Name, GUID: f.GUID, UIDValidity: f.UIDValidity}
		}
	}
	return out, nil
}

// resolver hands a pass the folders of the personal namespace a configuration
// names, their messages, and which of them a rule keeps.
type resolver struct {
	u   *userMailbox
	box mailbox.Box
	ids map[[16]byte]uint64
}

func (r *resolver) Folders(cfg *Config) ([]Backing, error) {
	sep := mailbox.SepOrDefault(r.u.info.Personal.Separator)
	entries, err := r.box.Store().ListFolders()
	if err != nil {
		return nil, fmt.Errorf("virtual: list folders: %w", err)
	}
	var out []Backing
	for _, e := range entries {
		name := SameSeparator(e.Name, sep)
		if !Selects(cfg, name) {
			continue
		}
		f, ferr := mailbox.Counting(r.box).Folder(e.Name, 0)
		if ferr != nil {
			continue // a folder that cannot be opened contributes nothing
		}
		keep, merr := PassesMetadata(cfg, name, r.annotation(f.GUID))
		if merr != nil {
			return nil, fmt.Errorf("virtual: %w", merr)
		}
		if !keep {
			continue
		}
		if r.ids == nil {
			r.ids = map[[16]byte]uint64{}
		}
		r.ids[f.GUID] = f.ID
		out = append(out, Backing{Name: name, GUID: f.GUID, UIDValidity: f.UIDValidity, NextUID: f.NextUID, HighestModSeq: f.HighestModSeq})
	}
	return out, nil
}

// annotation reads a personal folder's METADATA entry through the dict METADATA
// keeps it in.
func (r *resolver) annotation(guid [16]byte) MetadataLookup {
	return func(entry string) (string, bool, error) {
		md := r.u.opts.MetadataDict
		if md == nil {
			return "", false, nil
		}
		scope, attr, err := mailbox.ParseAttrEntry(entry)
		if err != nil {
			return "", false, err
		}
		p := r.u.info.Personal
		vals, found, err := md.Lookup(context.Background(), &dict.OpSettings{Username: p.Username, HomeDir: p.Home}, mailbox.AttrKey(scope, guid, attr))
		if err != nil || !found || len(vals) == 0 {
			return "", false, err
		}
		return string(vals[0]), true, nil
	}
}

func (r *resolver) Messages(b Backing) ([]*mailbox.MessageMeta, error) {
	id, ok := r.ids[b.GUID]
	if !ok {
		return nil, fmt.Errorf("virtual: %s was not listed", b.Name)
	}
	tx, err := r.box.Begin(id)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	msgs, err := tx.Messages(mailbox.SeqSet{})
	if err != nil {
		return nil, fmt.Errorf("virtual: read %s: %w", b.Name, err)
	}
	return msgs, nil
}

// Matches runs a rule over a backing folder with the evaluator SEARCH uses, so
// a rule means here what it means on the wire.
func (r *resolver) Matches(b Backing, rule string) (map[uint32]bool, error) {
	f := &mailbox.Folder{Name: b.Name, GUID: b.GUID, UIDValidity: b.UIDValidity}
	return r.u.opts.Search.Folder(r.box, r.u.info.Personal.Username, f, b.Messages, rule)
}
