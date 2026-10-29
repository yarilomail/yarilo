// Package dboxconv converts another implementation's dbox store into ours in
// place: their map entries become ours, pointing at the same files at the same
// offsets. One-way by design -- afterwards the original server would rebuild
// and lose flags and keywords (#1524).
package dboxconv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// Their file names, in one place: the removal step must name exactly the set
// the reading step understood.
const dboxMailsDir = "dbox-Mails"

const foreignSubscriptions = "subscriptions"

const (
	foreignIndex      = "dovecot.index"
	foreignLog        = "dovecot.index.log"
	foreignLogPrev    = "dovecot.index.log.2"
	foreignCache      = "dovecot.index.cache"
	foreignMapIndex   = "dovecot.map.index"
	foreignMapLog     = "dovecot.map.index.log"
	foreignMapLogPrev = "dovecot.map.index.log.2"
	// The message-file prefix both implementations write for sdbox.
	sdboxPrefix = "u."
)

// StoreRoot is where a dbox store lives, the rule the drivers apply: mail_path
// when set, else <home>/<driver>. One level out puts the map where nothing looks.
func StoreRoot(home, mailPath, driver string) string {
	if mailPath != "" {
		return mailPath
	}
	// The drivers disagree about which name they root under.
	switch driver {
	case "sdbox", "dbox":
		return filepath.Join(home, "sdbox")
	default:
		return filepath.Join(home, "mdbox")
	}
}

// HasForeignFolder reports whether dir holds their folder index. Either file:
// a folder written but not yet flushed has only the log (#1564).
func HasForeignFolder(dir string) bool {
	for _, name := range []string{foreignIndex, foreignLog} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// HasForeignMap reports whether a store's storage directory holds their map.
func HasForeignMap(storageDir string) bool {
	_, err := os.Stat(filepath.Join(storageDir, foreignMapLog))
	return err == nil
}

// AnyForeignFolderLeft reports whether an unconverted folder of theirs is left.
// It walks directories, not their list index, which answers a different
// question -- what they believed existed.
func AnyForeignFolderLeft(mailboxesDir string) (bool, error) {
	found := false
	err := filepath.WalkDir(mailboxesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if found {
			return fs.SkipAll
		}
		// Every directory, not only the dbox-Mails leaves: with a separate
		// index root their index sits in the mailbox directory itself (#1583).
		if !d.IsDir() {
			return nil
		}
		// Their FTS index carries index files of its own and is no folder (#2254).
		if d.Name() == mailbox.FTSIndexDir {
			return fs.SkipDir
		}
		if HasForeignFolder(p) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("dboxconv: walk %s: %w", mailboxesDir, err)
	}
	return found, nil
}

// DropForeignMapIfDone removes their map once no folder of theirs is left. An
// unconverted folder still addresses its mail through their uids, so only the
// last folder may end it (#1569); check and removal are one section under the
// map's lock, which orders a deleter against an importer.
func DropForeignMapIfDone(storageDir, mailboxesDir, mailRoot string, ours *mdboxmap.Map) (bool, error) {
	dropped := false
	err := ours.WithLock(func() error {
		left, err := AnyForeignFolderLeft(mailboxesDir)
		if err != nil {
			return err
		}
		if left {
			return nil
		}
		// The rotated log too: a file of theirs left behind is one a tool of
		// theirs would still read, and read as authoritative.
		for _, name := range []string{foreignMapIndex, foreignMapLog, foreignMapLogPrev} {
			if err := os.Remove(filepath.Join(storageDir, name)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("dboxconv: remove %s: %w", name, err)
			}
		}
		// Their subscription file is store-wide too, and its contents are in
		// ours by now -- carried on the first folder that converted.
		if err := os.Remove(filepath.Join(mailRoot, foreignSubscriptions)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("dboxconv: remove %s: %w", foreignSubscriptions, err)
		}
		dropped = true
		return nil
	})
	return dropped, err
}

// ErrReadOnly says a store cannot be converted because it cannot be written to.
var ErrReadOnly = errors.New("store is read-only; conversion deletes the foreign index and cannot run -- mount it writable or convert it offline")

// CheckWritable reports whether a conversion could finish, by writing to every
// directory it would write to -- before any work, and by writing rather than
// reasoning, since a read-only mount under 0700 bits defeats a permission check.
// Every directory: a mixed mount would otherwise fail halfway.
func CheckWritable(dirs ...string) error {
	for _, dir := range dirs {
		if err := checkWritable(dir); err != nil {
			return err
		}
	}
	return nil
}

func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".yarilo-convert-probe-*")
	if err != nil {
		return fmt.Errorf("dboxconv: %s: %w (%v)", dir, ErrReadOnly, err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("dboxconv: %s: %w (%v)", dir, ErrReadOnly, err)
	}
	return nil
}

// HasForeignSubscriptions reports whether the store carries their subscription
// file.
func HasForeignSubscriptions(mailRoot string) bool {
	_, err := os.Stat(filepath.Join(mailRoot, foreignSubscriptions))
	return err == nil
}

// ReadForeignMap reads their map: base then log, or the logs alone -- their base
// is written only past a rewrite threshold, so a young store has none.
func ReadForeignMap(storageDir string) ([]dboxindex.MapEntry, error) {
	var (
		seed     []dboxindex.MapEntry
		exts     []dboxindex.Extension
		baseSeq  uint32
		baseTail uint32
	)
	if raw, err := os.ReadFile(filepath.Join(storageDir, foreignMapIndex)); err == nil {
		h, herr := dboxindex.ParseHeader(raw)
		if herr != nil {
			return nil, fmt.Errorf("dboxconv: map index: %w", herr)
		}
		exts, herr = dboxindex.ParseExtensions(raw, h)
		if herr != nil {
			return nil, fmt.Errorf("dboxconv: map extensions: %w", herr)
		}
		// Base plus log-from-tail: after a rotation the base is the only place
		// the older half exists (#1583).
		seed, herr = dboxindex.ParseMapRecords(raw, h, exts)
		if herr != nil {
			return nil, fmt.Errorf("dboxconv: map records: %w", herr)
		}
		baseSeq, baseTail = h.LogFileSeq, h.LogFileTailOffset
	}

	chain, err := logChain(storageDir, foreignMapLog, foreignMapLogPrev, baseSeq, baseTail)
	if err != nil {
		return nil, fmt.Errorf("dboxconv: map log: %w", err)
	}
	for _, seg := range chain {
		seed, exts, err = dboxindex.ReadMapOntoExt(seed, seg.b, seg.at, exts)
		if err != nil {
			return nil, fmt.Errorf("dboxconv: map log %s: %w", seg.name, err)
		}
	}
	return seed, nil
}

// ErrHistoryLost says neither the base's own log nor the start of the history is
// on disk, so replaying what is left gives a short map or folder (#2253).
var ErrHistoryLost = errors.New("neither the base's log nor the start of the history is left")

// logSegment is one log of a chain and the offset its replay starts at.
type logSegment struct {
	name string
	b    []byte
	at   int
}

// logChain returns the logs to replay after a base at (baseSeq, baseTail), oldest
// first: from the base's tail, else from the start of the history (#2253).
func logChain(dir, cur, prev string, baseSeq, baseTail uint32) ([]logSegment, error) {
	raw, err := os.ReadFile(filepath.Join(dir, cur))
	if err != nil {
		return nil, err
	}
	lh, err := dboxindex.ParseLogHeader(raw)
	if err != nil {
		return nil, fmt.Errorf("%s header: %w", cur, err)
	}
	head := logSegment{name: cur, b: raw, at: int(lh.HeaderSize)}
	if baseSeq != 0 && baseSeq == lh.FileSeq {
		head.at = max(head.at, int(baseTail))
		return []logSegment{head}, nil
	}

	pb, err := os.ReadFile(filepath.Join(dir, prev))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var ph dboxindex.LogHeader
	if err == nil {
		if ph, err = dboxindex.ParseLogHeader(pb); err != nil {
			return nil, fmt.Errorf("%s header: %w", prev, err)
		}
	}
	older := logSegment{name: prev, b: pb, at: int(ph.HeaderSize)}
	continues := pb != nil && ph.FileSeq == lh.PrevFileSeq
	if baseSeq != 0 && continues && ph.FileSeq == baseSeq {
		older.at = max(older.at, int(baseTail))
		return []logSegment{older, head}, nil
	}
	// The whole history replays onto any base: what the base absorbed lands as it was.
	switch {
	case lh.PrevFileSeq == 0:
		return []logSegment{head}, nil
	case continues && ph.PrevFileSeq == 0:
		return []logSegment{older, head}, nil
	}
	return nil, fmt.Errorf("%s continues log %d, the base names log %d: %w", cur, lh.PrevFileSeq, baseSeq, ErrHistoryLost)
}

// Folder is one of their folders as read from disk: what it holds, how to
// locate it, and the two header fields that say which UID space it is.
type Folder struct {
	Records []dboxindex.Record
	Exts    []dboxindex.Extension
	Header  dboxindex.HeaderState
}

// ReadForeignFolder reads one folder's state: their base plus its log from
// log_file_tail_offset, or the log alone. Neither is an error, not an empty
// result that would read as a folder holding nothing.
func ReadForeignFolder(dir string) (Folder, error) {
	raw, err := os.ReadFile(filepath.Join(dir, foreignIndex))
	if err != nil {
		if !os.IsNotExist(err) {
			return Folder{}, fmt.Errorf("dboxconv: read index %s: %w", dir, err)
		}
		return folderFromLog(dir)
	}

	h, err := dboxindex.ParseHeader(raw)
	if err != nil {
		return Folder{}, fmt.Errorf("dboxconv: index %s: %w", dir, err)
	}
	recs, err := dboxindex.ParseRecords(raw, h)
	if err != nil {
		return Folder{}, fmt.Errorf("dboxconv: records %s: %w", dir, err)
	}
	exts, err := dboxindex.ParseExtensions(raw, h)
	if err != nil {
		return Folder{}, fmt.Errorf("dboxconv: extensions %s: %w", dir, err)
	}

	var names []string
	if kw, ok := dboxindex.Find(exts, "keywords"); ok {
		names, err = dboxindex.KeywordNames(kw)
		if err != nil {
			return Folder{}, fmt.Errorf("dboxconv: keywords %s: %w", dir, err)
		}
		for i := range recs {
			recs[i].Keywords = dboxindex.KeywordsOf(recs[i].Raw, kw, names)
		}
	}
	state := dboxindex.HeaderState{UIDValidity: h.UIDValidity, NextUID: h.NextUID}
	chain, err := logChain(dir, foreignLog, foreignLogPrev, h.LogFileSeq, h.LogFileTailOffset)
	if err != nil && !os.IsNotExist(err) {
		return Folder{}, fmt.Errorf("dboxconv: log %s: %w", dir, err)
	}
	for _, seg := range chain {
		changes, cerr := dboxindex.ReadChanges(seg.b, seg.at, exts)
		if cerr != nil {
			return Folder{}, fmt.Errorf("dboxconv: log %s/%s: %w", dir, seg.name, cerr)
		}
		state = dboxindex.ApplyHeader(state, changes)
		recs = dboxindex.Apply(recs, changes, names)
	}
	return Folder{Records: recs, Exts: exts, Header: state}, nil
}

// folderFromLog builds a folder from its logs alone, for one with no base yet.
// Extensions come from the logs' intro records: nothing else names the map uid
// an mdbox message is found through.
func folderFromLog(dir string) (Folder, error) {
	chain, err := logChain(dir, foreignLog, foreignLogPrev, 0, 0)
	if os.IsNotExist(err) {
		return Folder{}, fmt.Errorf("dboxconv: %s has no index and no log: %w", dir, err)
	}
	if err != nil {
		return Folder{}, fmt.Errorf("dboxconv: log %s: %w", dir, err)
	}
	var (
		all  []dboxindex.Change
		exts []dboxindex.Extension
	)
	for _, seg := range chain {
		changes, known, cerr := dboxindex.ReadChangesAndExtensions(seg.b, seg.at, exts)
		if cerr != nil {
			return Folder{}, fmt.Errorf("dboxconv: log %s/%s: %w", dir, seg.name, cerr)
		}
		all, exts = append(all, changes...), known
	}
	// No keyword table without a base: a keyword named in the log arrives, one
	// carried as a bitmask alone has nothing to resolve against.
	return Folder{
		Records: dboxindex.Apply(nil, all, nil),
		Exts:    exts,
		Header:  dboxindex.ApplyHeader(dboxindex.HeaderState{}, all),
	}, nil
}
