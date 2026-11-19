package virtual

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/search"
	"github.com/yarilomail/yarilo/v2/pkg/dict"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// Options is what a virtual namespace draws on: the user's personal mail, the
// search evaluator, the annotations a rule may test, and the sync lock.
type Options struct {
	// Personal opens a user's personal namespace, as its assembler builds it.
	Personal     func(*mailbox.UserInfo) mailbox.Box
	Search       search.Options
	MetadataDict dict.Dict
	Locker       locks.Locker
	// Shared opens, for one user, the namespaces besides the personal one a
	// pattern may reach, each as a session opens it (#2603).
	Shared func(*mailbox.UserInfo) []Source
	// Declared is every namespace but the personal one, with a store or not.
	Declared []Declared
}

// Declared is a configured namespace as a pattern resolves to it: its prefix
// in "/" form, an owner template (user/%u/) or a fixed one.
type Declared struct {
	Prefix         string
	OwnerTemplated bool
	HasStore       bool
}

// Source is the store of a namespace besides the personal one, for one user.
type Source struct {
	// Namespace is the Declared prefix a pattern binds to; Prefix and Sep
	// spell a folder's IMAP name, Prefix + its name in Box (one owner's, for
	// an owner-templated namespace).
	Namespace string
	Prefix    string
	Sep       string
	Box       mailbox.Box
	// Rights are the user's on a folder, from the ACL code LIST and SELECT use.
	Rights func(rel string) (mailbox.Rights, error)
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

// sources is the other namespaces this user's virtual mailboxes may draw from,
// opened with the personal mail and closed with this handle.
func (u *userMailbox) sources() []Source {
	if u.opts.Shared == nil || u.info.Personal == nil {
		return nil
	}
	u.personalMu.Lock()
	defer u.personalMu.Unlock()
	if !u.sharedOpen {
		u.shared, u.sharedOpen = u.opts.Shared(u.info.Personal), true
	}
	return u.shared
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
	r := &resolver{u: u, box: box, shared: u.sources()}
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
	for _, src := range u.sources() {
		entries, err := src.Box.Store().ListFolders()
		if err != nil {
			return nil, fmt.Errorf("virtual: list folders of %s: %w", src.Prefix, err)
		}
		for _, e := range entries {
			if rights, rerr := src.Rights(e.Name); rerr != nil || !rights.Has(mailbox.RightLookup) || !rights.Has(mailbox.RightRead) {
				continue
			}
			f, ferr := mailbox.Counting(src.Box).Folder(e.Name, 0)
			if ferr != nil {
				continue
			}
			if id, ok := byGUID[f.GUID]; ok {
				out[id] = mailbox.BackingRef{Name: e.Name, GUID: f.GUID, UIDValidity: f.UIDValidity, FullName: src.Prefix + e.Name}
			}
		}
	}
	return out, nil
}

// resolver hands a pass the folders a configuration names, their messages,
// and which of them a rule keeps.
type resolver struct {
	u      *userMailbox
	box    mailbox.Box
	shared []Source
	ids    map[[16]byte]listed
}

// listed is where a folder a pass selected is read from.
type listed struct {
	box    mailbox.Box
	id     uint64
	rel    string
	shared bool
}

func (r *resolver) list(f *mailbox.Folder, box mailbox.Box, rel string, shared bool) {
	if r.ids == nil {
		r.ids = map[[16]byte]listed{}
	}
	r.ids[f.GUID] = listed{box: box, id: f.ID, rel: rel, shared: shared}
}

func (r *resolver) Folders(cfg *Config) ([]Backing, error) {
	bound, err := r.bind(cfg)
	if err != nil {
		return nil, err
	}
	sep := mailbox.SepOrDefault(r.u.info.Personal.Separator)
	entries, err := r.box.Store().ListFolders()
	if err != nil {
		return nil, fmt.Errorf("virtual: list folders: %w", err)
	}
	here := func(p string) bool { return bound[p] == "" }
	var out []Backing
	for _, e := range entries {
		name := SameSeparator(e.Name, sep)
		if ok, _ := SelectsIn(cfg, name, here); !ok {
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
		r.list(f, r.box, e.Name, false)
		out = append(out, Backing{Name: name, GUID: f.GUID, UIDValidity: f.UIDValidity, NextUID: f.NextUID, HighestModSeq: f.HighestModSeq})
	}
	for _, src := range r.shared {
		got, err := r.sharedFolders(cfg, src, bound)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// bind resolves each line's pattern to its namespace, "" for the personal one
// (INBOX included): the longest declared prefix the pattern starts with. A
// pattern under a namespace with no store is a configuration error.
func (r *resolver) bind(cfg *Config) (map[string]string, error) {
	out := make(map[string]string, len(cfg.Boxes))
	for _, b := range cfg.Boxes {
		if b.MetadataEntry != "" {
			continue
		}
		best := Declared{}
		if !strings.EqualFold(b.Pattern, "INBOX") {
			for _, d := range r.u.opts.Declared {
				if d.Prefix != "" && prefixMatches(d, b.Pattern) && len(d.Prefix) > len(best.Prefix) {
					best = d
				}
			}
		}
		if best.Prefix != "" && !best.HasStore {
			return nil, fmt.Errorf("virtual: pattern %q: namespace %q has no store", b.Pattern, best.Prefix)
		}
		out[b.Pattern] = best.Prefix
	}
	return out, nil
}

// prefixMatches says pattern starts with d's prefix; an owner template's
// variable segment takes any one segment, a wildcard included.
func prefixMatches(d Declared, pattern string) bool {
	if !d.OwnerTemplated {
		return strings.HasPrefix(pattern, d.Prefix)
	}
	want := strings.Split(strings.TrimSuffix(d.Prefix, "/"), "/")
	got := strings.SplitN(pattern, "/", len(want)+1)
	if len(got) <= len(want) {
		return false
	}
	for i, w := range want {
		if !strings.Contains(w, "%") && w != got[i] {
			return false
		}
	}
	return true
}

// sharedFolders is Folders over one other namespace's store. A folder the user
// may not open is skipped when a wildcard took it and fails the pass when a
// line names it (#2603).
func (r *resolver) sharedFolders(cfg *Config, src Source, bound map[string]string) ([]Backing, error) {
	entries, err := src.Box.Store().ListFolders()
	if err != nil {
		return nil, fmt.Errorf("virtual: list folders of %s: %w", src.Prefix, err)
	}
	here := func(p string) bool { return bound[p] == src.Namespace }
	var out []Backing
	for _, e := range entries {
		name := SameSeparator(src.Prefix+e.Name, mailbox.SepOrDefault(src.Sep))
		ok, explicit := SelectsIn(cfg, name, here)
		if !ok {
			continue
		}
		rights, rerr := src.Rights(e.Name)
		if rerr != nil || !rights.Has(mailbox.RightLookup) || !rights.Has(mailbox.RightRead) {
			if explicit {
				return nil, fmt.Errorf("virtual: %s is named but may not be read: %v", name, rerr)
			}
			continue
		}
		f, ferr := mailbox.Counting(src.Box).Folder(e.Name, 0)
		if ferr != nil {
			continue // not selectable or gone
		}
		keep, merr := PassesMetadata(cfg, name, r.sharedAnnotation(f.GUID))
		if merr != nil {
			return nil, fmt.Errorf("virtual: %w", merr)
		}
		if !keep {
			continue
		}
		r.list(f, src.Box, e.Name, true)
		out = append(out, Backing{Name: name, GUID: f.GUID, UIDValidity: f.UIDValidity, NextUID: f.NextUID, HighestModSeq: f.HighestModSeq})
	}
	return out, nil
}

// sharedAnnotation reads another namespace's folder's METADATA entry under the
// key a session's GETMETADATA reads it by.
func (r *resolver) sharedAnnotation(guid [16]byte) MetadataLookup {
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
		vals, found, err := md.Lookup(context.Background(), &dict.OpSettings{Username: p.Username, HomeDir: p.Home}, mailbox.SharedAttrKey(scope, guid, p.Username, attr))
		if err != nil || !found || len(vals) == 0 {
			return "", false, err
		}
		return string(vals[0]), true, nil
	}
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
	at, ok := r.ids[b.GUID]
	if !ok {
		return nil, fmt.Errorf("virtual: %s was not listed", b.Name)
	}
	tx, err := at.box.Begin(at.id)
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
	at, ok := r.ids[b.GUID]
	if !ok {
		return nil, fmt.Errorf("virtual: %s was not listed", b.Name)
	}
	f := &mailbox.Folder{Name: b.Name, GUID: b.GUID, UIDValidity: b.UIDValidity}
	o := r.u.opts.Search
	if at.shared {
		// The user's text index does not hold another store's folder.
		f.Name, o.Client = at.rel, nil
	}
	return o.Folder(at.box, r.u.info.Personal.Username, f, b.Messages, rule)
}
