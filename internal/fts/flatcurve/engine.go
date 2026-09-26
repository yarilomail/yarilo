//go:build flatcurve

package flatcurve

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/0kaba0hub/go-xapian"

	"github.com/yarilomail/yarilo/internal/fts/ftsstore"
	"github.com/yarilomail/yarilo/pkg/fts"
)

// On-disk format constants (see https://doc.yarilomail.org/FTS for the format specification).
const (
	Label         = "fts-flatcurve"
	dbPrefix      = "index."
	currentPrefix = "current."
	versionKey    = "yarilo.fts-flatcurve"
	versionValue  = "1"

	allHdrPrefix = "A"
	boolPrefix   = "B"
	hdrPrefix    = "H"

	// maxTermBytes: the glass backend caps terms at ~245 bytes; the format
	// truncates at 200.
	maxTermBytes = 200

	// checkpointFile is a yarilo sidecar (not part of the flatcurve format):
	// per-mailbox last_indexed_uid + settings checksum. Missing on a migrated
	// index, it falls back to Xapian's get_lastdocid.
	checkpointFile = "yarilo.checkpoint"
)

// indexedHeaders are the header fields that get their own H<NAME> terms.
var indexedHeaders = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true, "subject": true,
}

// Options are the fts_flatcurve_* tunables with upstream defaults.
type Options struct {
	CommitLimit int // fts_flatcurve_commit_limit (500)
	MinTermSize int // fts_flatcurve_min_term_size (2)
	// PrefixSearch decides which terms a search expands as prefixes
	// (fts_flatcurve_prefix_search): "yes", "no", "N" or "N-M". Empty means
	// "yes", which is what the engine did before the setting existed.
	PrefixSearch    string
	OptimizeLimit   int           // fts_flatcurve_optimize_limit (10; 0 = disabled)
	RotateCount     uint32        // fts_flatcurve_rotate_count (5000)
	RotateTime      time.Duration // fts_flatcurve_rotate_time (5000ms; 0 = disabled)
	SubstringSearch bool          // fts_flatcurve_substring_search (no)

	// Store is where the indexes live. The service builds it from
	// fts_index_root; nil means a posix store over this engine's layout, which
	// is what an engine constructed without one has always used.
	Store fts.IndexStore
}

func (o Options) withDefaults() Options {
	if o.CommitLimit <= 0 {
		o.CommitLimit = 500
	}
	if o.MinTermSize <= 0 {
		o.MinTermSize = 2
	}
	// OptimizeLimit has no default here: 0 means "auto-optimize disabled". The
	// positive default (10) lives in pkg/config.DefaultConfig() only, so a
	// config layer explicitly setting 0 is respected, not overridden.
	if o.RotateCount == 0 {
		o.RotateCount = 5000
	}
	// RotateTime has no default here: 0 means "time-based rotation disabled".
	// The positive default (5000ms) lives in pkg/config.DefaultConfig() only.
	if o.Store == nil {
		o.Store = ftsstore.NewPosix(Layout(), "")
	}
	return o
}

// Layout is this engine's on-disk shape, handed to whichever store keeps the
// indexes. The current shape is keyed by the folder's GUID, which is its
// identity: a rename keeps it and a driver migration does not change it, so
// neither orphans the index. The mail driver's layout is the mail tree's
// business and has none here -- the FTS root is its own tree (#1183).
//
// The legacy shapes are the ones this engine wrote before, newest first: the
// driver-aware one, then the original flat one. Both are keyed by the folder
// NAME, which is why a rename orphaned them.
//
// UserRef keeps Driver / Separator / EscapeChar for this: the GUID-keyed path
// needs none of them, but reading where an older index sits does, and will
// for as long as any deployment can still be carrying one.
func Layout() fts.Layout {
	// One index per user: a document carries the folders it belongs to as
	// terms, so there is nothing per folder to place (#1986).
	return fts.Layout{
		Dir: func(root string, _ fts.UserRef) string {
			return filepath.Join(root, Label)
		},
	}
}

// Engine implements fts.Engine over Xapian.
type Engine struct {
	opts Options
	// prefix is the parsed PrefixSearch setting, resolved once at construction
	// so the query path does not re-parse a string per term.
	prefix PrefixRange

	// optimizeCB implements fts.OptimizeNotifier. Set once at startup before
	// any indexing; stored via atomic.Pointer so every userIndex write path
	// (its own goroutine, its own per-user lock) can read it without a shared
	// mutex.
	optimizeCB atomic.Pointer[func(fts.UserRef, fts.MailboxRef)]
}

// New returns a flatcurve engine.
//
// An unparseable or unworkable PrefixSearch falls back to expanding every term,
// which is what the engine did before the setting existed, and says so. A
// search engine that silently narrowed its own matching would be reported as
// missing mail.
func New(opts Options) *Engine {
	o := opts.withDefaults()
	prefix, err := ParsePrefixRange(o.PrefixSearch)
	if err != nil {
		slog.Error("fts/flatcurve: prefix search setting unusable, expanding every term",
			"setting", o.PrefixSearch, "err", err)
		prefix = PrefixRange{Enabled: true}
	}
	// Substring indexing stores suffixes of every term, and a suffix is only
	// reachable by expanding the query as a prefix. The two settings are one
	// mechanism seen from both ends: with expansion off, substring indexing
	// writes terms nothing can ask for.
	if o.SubstringSearch && !prefix.Enabled {
		slog.Error("fts/flatcurve: substring search needs prefix expansion; expanding every term",
			"substring_search", true, "prefix_search", o.PrefixSearch)
		prefix = PrefixRange{Enabled: true}
	}
	return &Engine{opts: o, prefix: prefix}
}

// SetOptimizeCallback implements fts.OptimizeNotifier.
func (e *Engine) SetOptimizeCallback(fn func(user fts.UserRef, mbox fts.MailboxRef)) {
	e.optimizeCB.Store(&fn)
}

// notifyOptimizeIfNeeded checks the sealed-shard count right after a rotation
// and, once it reaches OptimizeLimit, calls the registered callback. Called
// under the owning userIndex's u.mu; the callback must only enqueue and
// return, never compact, so this stays a bounded check (one os.ReadDir) and
// doesn't extend how long the write path holds the lock. Firing on every
// rotation while still at/above the limit is harmless: the queue dedups.
func (e *Engine) notifyOptimizeIfNeeded(st *mboxState) {
	if e.opts.OptimizeLimit <= 0 {
		return
	}
	cb := e.optimizeCB.Load()
	if cb == nil {
		return
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return
	}
	if len(paths) >= e.opts.OptimizeLimit {
		(*cb)(st.user, st.mbox)
	}
}

func (e *Engine) Name() string { return "flatcurve" }

func (e *Engine) Caps() fts.Caps {
	return fts.Caps{
		Tokenized: true,
		Scoring:   true,
		Substring: e.opts.SubstringSearch,
	}
}

func (e *Engine) Close() error { return nil }

func (e *Engine) OpenUser(_ context.Context, user fts.UserRef) (fts.UserIndex, error) {
	return &userIndex{eng: e, user: user}, nil
}

// mboxState holds the open write shard for one mailbox. The service is the
// sole writer (https://doc.yarilomail.org/FTS §4), so a plain mutex per user index suffices.
type mboxState struct {
	dir     string
	eng     *Engine        // gives commitCurrent access to opts.RotateTime without threading it through every call site
	user    fts.UserRef    // owning user, for the optimize callback
	mbox    fts.MailboxRef // this mailbox, for the optimize callback
	cur     *xapian.WDB
	curPath string
	pending int    // uncommitted document updates
	curDocs uint32 // documents written to the current shard
}

type userIndex struct {
	eng  *Engine
	user fts.UserRef
	mu   sync.Mutex
	box  *mboxState
}

// state is the user's one index: a document names the folders it belongs to,
// so there is no per-folder state to keep (#1986).
func (u *userIndex) state() *mboxState {
	if u.box != nil {
		return u.box
	}
	dir := u.eng.opts.Store.Locate(u.user)
	// Prepare adopts an index left at an older layout; it answers with the
	// same location Locate did.
	if prepared, err := u.eng.opts.Store.Prepare(u.user); err == nil {
		dir = prepared
	} else {
		slog.Warn("fts/flatcurve: preparing the index location failed; using it as-is",
			"dir", dir, "err", err)
	}
	cleanStaleOptimizeTmp(dir)
	u.box = &mboxState{dir: dir, eng: u.eng, user: u.user}
	return u.box
}

// cleanStaleOptimizeTmp removes a leftover "optimize" compaction tmp directory
// from a crash mid-run. optimizeDir already os.RemoveAll()s this path before a
// fresh Compact, so a stale one is at worst wasted disk, never corruption;
// this reclaims it the first time the directory is touched, since the service
// has no upfront list of mailboxes to sweep at startup. shardPaths only picks
// up dbPrefix/currentPrefix dirs, so a stale one is never taken for a shard.
func cleanStaleOptimizeTmp(dir string) {
	tmp := filepath.Join(dir, "optimize")
	if _, err := os.Stat(tmp); err != nil {
		return
	}
	if err := os.RemoveAll(tmp); err != nil {
		slog.Warn("fts/flatcurve: failed to clean stale optimize tmp dir", "dir", tmp, "err", err)
		return
	}
	slog.Info("fts/flatcurve: cleaned stale optimize tmp dir left over from a prior crash", "dir", tmp)
}

func shardPaths(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fts/flatcurve: readdir: %w", err)
	}
	var out []string
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, dbPrefix) || strings.HasPrefix(name, currentPrefix) {
			out = append(out, filepath.Join(dir, name))
		}
	}
	sort.Strings(out)
	return out, nil
}

func (st *mboxState) ensureCurrent() error {
	if st.cur != nil {
		return nil
	}
	if err := st.eng.opts.Store.Create(st.dir); err != nil {
		return fmt.Errorf("fts/flatcurve: mkdir: %w", err)
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return err
	}
	var curPath string
	for _, p := range paths {
		if strings.HasPrefix(filepath.Base(p), currentPrefix) {
			curPath = p // sorted: keep the highest suffix
		}
	}
	fresh := curPath == ""
	if fresh {
		curPath = filepath.Join(st.dir,
			fmt.Sprintf("%s%d", currentPrefix, time.Now().UnixMicro()))
	}
	// Create the shard directory and fsync its parent BEFORE handing the path
	// to Xapian. Xapian opens the glass DB with DB_NO_SYNC (no directory
	// fsync), so letting it create current.<ts> leaves the directory entry
	// unflushed: a rotate/rename or restart then races a write into a
	// not-yet-durable directory, surfacing as "Couldn't write new rev file:
	// .../current.<ts>/v.tmp (No such file or directory)" and permanently
	// wedging the shard. Making + fsyncing the directory first closes that gap.
	if err := os.MkdirAll(curPath, 0o700); err != nil {
		return fmt.Errorf("fts/flatcurve: mkdir current shard: %w", err)
	}
	if err := st.eng.opts.Store.Sync(st.dir); err != nil {
		return fmt.Errorf("fts/flatcurve: fsync shard parent: %w", err)
	}
	slog.Debug("fts/flatcurve: ensureCurrent opening shard", "dir", st.dir, "cur_path", curPath, "fresh", fresh)
	w, err := xapian.OpenWDB(curPath)
	if err != nil {
		slog.Warn("fts/flatcurve: ensureCurrent open failed", "dir", st.dir, "cur_path", curPath, "fresh", fresh, "err", err)
		return err
	}
	if fresh {
		if err := w.SetMetadata(versionKey, versionValue); err != nil {
			w.Close()
			return err
		}
	}
	st.cur = w
	st.curPath = curPath
	n, err := w.DocCount()
	if err != nil {
		w.Close()
		st.cur = nil
		return err
	}
	st.curDocs = n
	return nil
}

// commitCurrent commits the pending documents in the current write shard. If
// the commit took longer than RotateTime, it rotates right after: large-
// document mailboxes with few messages per shard never hit RotateCount, so
// without this a single shard accumulates unbounded slow-to-commit data. Every
// caller (the CommitLimit check, an explicit Commit(), Refresh()) goes through
// here, so the time-based trigger applies uniformly.
func (st *mboxState) commitCurrent() error {
	if st.cur == nil || st.pending == 0 {
		slog.Debug("fts/flatcurve: commit skipped", "dir", st.dir, "cur_path", st.curPath,
			"open", st.cur != nil, "pending", st.pending, "st", fmt.Sprintf("%p", st))
		return nil
	}
	slog.Debug("fts/flatcurve: commit", "dir", st.dir, "cur_path", st.curPath,
		"pending", st.pending, "st", fmt.Sprintf("%p", st))
	t0 := time.Now()
	if err := st.cur.Commit(); err != nil {
		slog.Warn("fts/flatcurve: commitCurrent failed, discarding handle", "dir", st.dir, "cur_path", st.curPath, "pending", st.pending, "err", err)
		st.discardCurrent() // reopen next pass rather than keep a dead handle
		return err
	}
	dur := time.Since(t0)
	st.pending = 0
	if rt := st.eng.opts.RotateTime; rt > 0 && dur >= rt {
		slog.Debug("fts/flatcurve: commit exceeded rotate_time, rotating",
			"dir", st.dir, "cur_path", st.curPath, "commit_dur", dur, "rotate_time", rt)
		if err := st.rotate(); err != nil {
			st.discardCurrent()
			return err
		}
		st.eng.notifyOptimizeIfNeeded(st) // this seals a shard too
	}
	return nil
}

// rotate seals the current shard: current.### becomes index.### and the next
// write opens a fresh current shard.
func (st *mboxState) rotate() error {
	if st.cur == nil {
		return nil
	}
	if err := st.cur.Commit(); err != nil {
		slog.Warn("fts/flatcurve: rotate commit failed, discarding handle", "dir", st.dir, "cur_path", st.curPath, "err", err)
		st.discardCurrent() // reopen on the next pass
		return err
	}
	st.cur.Close()
	st.cur = nil
	st.pending = 0
	st.curDocs = 0
	sealed := filepath.Join(st.dir,
		fmt.Sprintf("%s%d", dbPrefix, time.Now().UnixMicro()))
	slog.Debug("fts/flatcurve: rotating shard", "dir", st.dir, "from", st.curPath, "to", sealed)
	if err := os.Rename(st.curPath, sealed); err != nil {
		slog.Warn("fts/flatcurve: rotate rename failed", "dir", st.dir, "from", st.curPath, "to", sealed, "err", err)
		return fmt.Errorf("fts/flatcurve: rotate: %w", err)
	}
	// Make the rename durable before the next ensureCurrent creates a new
	// current.<ts>: otherwise a restart could leave both the old (unrenamed)
	// and new shard directory entries unflushed, the state that wedges a shard.
	if err := st.eng.opts.Store.Sync(st.dir); err != nil {
		return fmt.Errorf("fts/flatcurve: fsync after rotate: %w", err)
	}
	st.curPath = ""
	return nil
}

// discardCurrent force-releases the write shard WITHOUT committing. Called on
// any engine error so the next ensureCurrent reopens a fresh handle instead of
// returning the same poisoned one forever (the DatabaseClosedError sticky
// handle). Uncommitted docs are dropped, but the caller returns the error so
// the ftsservice checkpoint does not advance and those UIDs re-index next pass.
// Best-effort: close errors are ignored (the handle is dead anyway).
func (st *mboxState) discardCurrent() {
	if st.cur != nil {
		st.cur.Close()
		st.cur = nil
	}
	st.pending = 0
	st.curDocs = 0
	st.curPath = ""
}

// closeCurrent commits and releases the write shard so other opens (expunge
// across shards, rescan, optimize, external readers) see a settled state.
func (st *mboxState) closeCurrent() error {
	if st.cur == nil {
		return nil
	}
	err := st.cur.Commit()
	st.cur.Close()
	st.cur = nil
	st.pending = 0
	st.curDocs = 0
	st.curPath = ""
	return err
}

/* --- checkpoints ---------------------------------------------------------- */

// Checkpoint returns the persisted (last_indexed_uid, uidvalidity, settings
// checksum). The on-disk file is v2 ("2 <uidvalidity> <last_uid> <checksum>");
// an old v1 file ("1 <last_uid> <checksum>") reads uidvalidity back as 0 so
// the caller treats it as "unknown" and lets a UIDVALIDITY mismatch reset it.
// checkpointPath names one folder's checkpoint inside the user's index: the
// index is one, the indexing progress is per folder (#1986).
func checkpointPath(dir string, mbox fts.MailboxRef) string {
	name := mbox.GUID
	if name == "" {
		name = "user"
	}
	return filepath.Join(dir, checkpointFile+"."+name)
}

func (u *userIndex) Checkpoint(mbox fts.MailboxRef) (lastUID, uidValidity, sum uint32, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	if data, rerr := os.ReadFile(checkpointPath(st.dir, mbox)); rerr == nil {
		var version uint32
		if _, e := fmt.Sscanf(string(data), "%d", &version); e == nil {
			switch version {
			case 2:
				if _, e2 := fmt.Sscanf(string(data), "%d %d %d %d", &version, &uidValidity, &lastUID, &sum); e2 == nil {
					return lastUID, uidValidity, sum, nil
				}
			case 1:
				if _, e2 := fmt.Sscanf(string(data), "%d %d %d", &version, &lastUID, &sum); e2 == nil {
					return lastUID, 0, sum, nil
				}
			}
		}
	}
	// No checkpoint: never indexed. The highest docid says nothing about a
	// uid now, so there is nothing to fall back on (#1986).
	return 0, 0, 0, nil
}

func (u *userIndex) SetCheckpoint(mbox fts.MailboxRef, lastUID, uidValidity, sum uint32) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	if err := st.eng.opts.Store.Create(st.dir); err != nil {
		return fmt.Errorf("fts/flatcurve: mkdir: %w", err)
	}
	tmp := checkpointPath(st.dir, mbox) + ".tmp"
	body := fmt.Sprintf("2 %d %d %d\n", uidValidity, lastUID, sum)
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return fmt.Errorf("fts/flatcurve: checkpoint write: %w", err)
	}
	if err := os.Rename(tmp, checkpointPath(st.dir, mbox)); err != nil {
		return fmt.Errorf("fts/flatcurve: checkpoint rename: %w", err)
	}
	return nil
}

/* --- indexing --------------------------------------------------------------- */

func (u *userIndex) BeginUpdate(mbox fts.MailboxRef) (fts.Update, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	return &update{ui: u, st: st, folder: mbox.GUID}, nil
}

// The document's terms: what it is, which folders hold a copy, and the address
// of each copy. INTERNALS §7 carries the table (#1986).
const (
	termGUID   = "G"
	termFolder = "XF"
	// slotGUID holds the message GUID, so a hit says which message it is
	// without the docid meaning anything.
	slotGUID = 0
)

func guidTerm(guid [16]byte) string { return termGUID + hex.EncodeToString(guid[:]) }

func folderTerm(folderGUID string) string { return termFolder + folderGUID }

type update struct {
	ui     *userIndex
	st     *mboxState
	folder string
	guid   [16]byte
	uid    uint32
	doc    *xapian.Doc
	key    fts.BuildKey
	// seenBool dedups header-existence terms within one document.
	seenBool map[string]bool
}

func (up *update) SetBuildKey(k fts.BuildKey) (bool, error) {
	if k.Type == fts.KeyBodyPartBinary {
		return false, nil
	}
	up.ui.mu.Lock()
	defer up.ui.mu.Unlock()
	if up.doc != nil && k.UID != up.uid {
		if err := up.flushDocLocked(); err != nil {
			return false, err
		}
	}
	if up.doc == nil {
		up.doc = xapian.NewDoc()
		up.seenBool = map[string]bool{}
		up.uid = k.UID
		up.guid = k.GUID
	}
	up.key = k
	// The header-existence boolean term is NOT set here: header presence is
	// recorded only together with at least one >=min_term_size token surviving
	// normTerm, so it's set lazily in BuildMore once that's known — HEADER
	// X-Foo "" (a value that tokenizes to nothing) must not match.
	return true, nil
}

// normTerm normalizes a term: minimum length, the 200-byte cap
// (multibyte-safe), and lowercasing the first ASCII character (Xapian treats a
// leading capital as a term prefix).
// normTerm drops a token below the minimum length and truncates one above the
// storage bound.
//
// The minimum counts characters, the bound counts bytes, and the difference is
// deliberate: the minimum is a judgement about what is worth indexing, which is
// the same judgement in every script, while the bound is about how much room a
// term takes on disk. Counting bytes for the minimum kept single-character
// Cyrillic and CJK terms while dropping single-character Latin ones (#1055).
func normTerm(tok string, minSize int) string {
	if utf8.RuneCountInString(tok) < minSize {
		return ""
	}
	if len(tok) > maxTermBytes {
		b := []byte(tok[:maxTermBytes])
		for len(b) > 0 {
			r, size := utf8.DecodeLastRune(b)
			if r != utf8.RuneError || size != 1 {
				break
			}
			b = b[:len(b)-1]
		}
		tok = string(b)
	}
	if len(tok) > 0 && tok[0] >= 'A' && tok[0] <= 'Z' {
		tok = string(tok[0]+('a'-'A')) + tok[1:]
	}
	return tok
}

// addWithSuffixes adds prefix+term and, in substring mode, every suffix of the
// term not shorter than minSize.
func (up *update) addWithSuffixes(prefix, term string, substring bool, minSize int) error {
	s := term
	for {
		if err := up.doc.AddTerm(prefix + s); err != nil {
			return err
		}
		if !substring {
			return nil
		}
		_, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if utf8.RuneCountInString(s) < minSize {
			return nil
		}
	}
}

func (up *update) BuildMore(data []byte) error {
	up.ui.mu.Lock()
	defer up.ui.mu.Unlock()
	if up.doc == nil {
		return fmt.Errorf("fts/flatcurve: BuildMore without a build key")
	}
	opts := up.ui.eng.opts
	term := normTerm(string(data), opts.MinTermSize)
	if term == "" {
		return nil
	}
	switch up.key.Type {
	case fts.KeyHeader, fts.KeyMIMEHeader:
		if err := up.addWithSuffixes(allHdrPrefix, term, opts.SubstringSearch, opts.MinTermSize); err != nil {
			return err
		}
		name := strings.ToLower(up.key.HdrName)
		if name == "" {
			// The header-NAME-only build key (empty HdrName) only reaches the
			// A-pool above; it has no per-field existence signal of its own.
			return nil
		}
		// Header existence: set lazily here, now that a real (>=min_term_size)
		// token for THIS field is confirmed, not proactively in SetBuildKey.
		if !up.seenBool[name] {
			up.seenBool[name] = true
			if err := up.doc.AddBooleanTerm(boolPrefix + name); err != nil {
				return err
			}
		}
		if indexedHeaders[name] {
			p := hdrPrefix + strings.ToUpper(name)
			return up.addWithSuffixes(p, term, opts.SubstringSearch, opts.MinTermSize)
		}
		return nil
	default: // body
		return up.addWithSuffixes("", term, opts.SubstringSearch, opts.MinTermSize)
	}
}

// writeDocLocked stores the message. A copy of a message the current shard
// already holds joins that document; the docid is the database's own.
func (up *update) writeDocLocked(st *mboxState) error {
	if err := up.doc.AddBooleanTerm(guidTerm(up.guid)); err != nil {
		return err
	}
	if err := up.doc.AddBooleanTerm(folderTerm(up.folder)); err != nil {
		return err
	}
	if err := up.doc.SetValue(slotGUID, string(up.guid[:])); err != nil {
		return err
	}
	ids, err := st.cur.DocIDsByTerm(guidTerm(up.guid))
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		return joinCopy(st.cur, ids[0], up.folder)
	}
	// A message indexed before this shard opened lives in a sealed one, and the
	// copy belongs to that document: the retraction already reaches there.
	joined, jerr := up.joinInSealedShard(st)
	if jerr != nil || joined {
		return jerr
	}
	_, aerr := st.cur.AddDocument(up.doc)
	return aerr
}

// joinInSealedShard adds the copy's terms to the message's document wherever it
// is, and reports whether it found one.
func (up *update) joinInSealedShard(st *mboxState) (bool, error) {
	paths, err := shardPaths(st.dir)
	if err != nil {
		return false, err
	}
	for _, p := range paths {
		if p == st.curPath && st.cur != nil {
			continue
		}
		// Read first: a new message is in no sealed shard, so the common answer
		// is a miss, and it must not cost a write handle on every delivery.
		db, oerr := xapian.OpenDBMulti([]string{p})
		if oerr != nil {
			return false, oerr
		}
		ids, derr := db.DocIDsByTerm(guidTerm(up.guid))
		db.Close()
		if derr != nil {
			return false, derr
		}
		if len(ids) == 0 {
			continue
		}
		metricSealedWriteOpen.Inc()
		w, werr := xapian.OpenWDB(p)
		if werr != nil {
			return false, werr
		}
		ids, derr = w.DocIDsByTerm(guidTerm(up.guid))
		if derr == nil && len(ids) > 0 {
			if derr = joinCopy(w, ids[0], up.folder); derr == nil {
				derr = w.Commit()
			}
		}
		found := derr == nil && len(ids) > 0
		w.Close()
		if derr != nil {
			return false, derr
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// joinCopy names the copy's folder on the message's document; a second copy in
// the same folder adds nothing, the folder being named already (#1986).
func joinCopy(w *xapian.WDB, id uint32, folderGUID string) error {
	stored, gerr := w.GetDocument(id)
	if gerr != nil {
		return gerr
	}
	defer stored.Free()
	if err := stored.AddBooleanTerm(folderTerm(folderGUID)); err != nil {
		return err
	}
	return w.ReplaceDocument(id, stored)
}

func (up *update) flushDocLocked() error {
	if up.doc == nil {
		return nil
	}
	st := up.st
	if err := st.ensureCurrent(); err != nil {
		return err
	}
	if err := up.writeDocLocked(st); err != nil {
		st.discardCurrent() // poisoned shard → reopen on the next pass
		return err
	}
	up.doc.Free()
	up.doc = nil
	up.seenBool = nil
	st.pending++
	st.curDocs++
	opts := up.ui.eng.opts
	if st.pending >= opts.CommitLimit {
		if err := st.commitCurrent(); err != nil {
			st.discardCurrent()
			return err
		}
	}
	if st.curDocs >= opts.RotateCount {
		if err := st.rotate(); err != nil {
			st.discardCurrent()
			return err
		}
		up.ui.eng.notifyOptimizeIfNeeded(st)
	}
	return nil
}

func (up *update) Commit() error {
	up.ui.mu.Lock()
	defer up.ui.mu.Unlock()
	if err := up.flushDocLocked(); err != nil {
		return err
	}
	return up.st.commitCurrent()
}

func (up *update) Rollback() error {
	up.ui.mu.Lock()
	defer up.ui.mu.Unlock()
	// Already-flushed documents stay (rescan reconciles); only the pending
	// document is discarded.
	if up.doc != nil {
		up.doc.Free()
		up.doc = nil
	}
	return nil
}

/* --- expunge / rescan / optimize -------------------------------------------- */

// Expunge retracts one copy by the message it was; without a per-copy term the
// store is what says whether this folder, or any, still holds one (#1986).
func (u *userIndex) Expunge(mbox fts.MailboxRef, guid [16]byte, inFolder, anywhere bool) error {
	if inFolder {
		// Another copy of the same message is still in this folder, so the
		// folder term stays: the document is the message.
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	if st.cur != nil {
		done, err := dropCopy(st.cur, mbox.GUID, guid, anywhere)
		if err != nil {
			return err
		}
		if done {
			st.pending++
			return st.commitCurrent()
		}
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return err
	}
	// A copy indexed before the current shard opened lives in a sealed one.
	for _, p := range paths {
		if p == st.curPath && st.cur != nil {
			continue
		}
		w, oerr := xapian.OpenWDB(p)
		if oerr != nil {
			return oerr
		}
		done, derr := dropCopy(w, mbox.GUID, guid, anywhere)
		if derr == nil && done {
			derr = w.Commit()
		}
		w.Close()
		if derr != nil {
			return derr
		}
		if done {
			return nil
		}
	}
	return nil
}

// dropCopy takes the folder off the message's document, and deletes the
// document when the store says no folder holds the message any more.
func dropCopy(w *xapian.WDB, folderGUID string, guid [16]byte, anywhere bool) (bool, error) {
	ids, err := w.DocIDsByTerm(guidTerm(guid))
	if err != nil || len(ids) == 0 {
		return false, err
	}
	if !anywhere {
		_, derr := w.DeleteDocument(ids[0])
		return true, derr
	}
	doc, gerr := w.GetDocument(ids[0])
	if gerr != nil {
		return false, gerr
	}
	defer doc.Free()
	if err := doc.RemoveTerm(folderTerm(folderGUID)); err != nil {
		return false, err
	}
	return true, w.ReplaceDocument(ids[0], doc)
}

// DropFolder retracts a deleted mailbox: a document no folder names goes with
// the terms (#2022).
func (u *userIndex) DropFolder(mbox fts.MailboxRef) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	if err := st.closeCurrent(); err != nil {
		return err
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := dropFolderInShard(p, mbox.GUID); err != nil {
			return err
		}
	}
	return nil
}

// dropFolderInShard reads the folder's postings first and opens for writing
// only on a hit: a delete costs the folder, not the whole index (#2017).
func dropFolderInShard(path, folderGUID string) error {
	db, err := xapian.OpenDBMulti([]string{path})
	if err != nil || db == nil {
		return err
	}
	ids, err := db.DocIDsByTerm(folderTerm(folderGUID))
	db.Close()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	metricSealedWriteOpen.Inc()
	w, err := xapian.OpenWDB(path)
	if err != nil {
		return err
	}
	defer w.Close()
	for _, id := range ids {
		if derr := dropFolderFromDoc(w, id, folderGUID); derr != nil {
			return derr
		}
	}
	return w.Commit()
}

// DropOrphanFolders is the healing half: a folder term naming a mailbox the
// account no longer has is one a deletion never reached (#2022).
func (u *userIndex) DropOrphanFolders(live []string) (int, error) {
	alive := make(map[string]struct{}, len(live))
	for _, g := range live {
		alive[g] = struct{}{}
	}
	dropped := 0
	err := u.dropFolders(func(folderGUID string) bool {
		_, ok := alive[folderGUID]
		if !ok {
			dropped++
		}
		return !ok
	})
	return dropped, err
}

// DocGUIDs is every message the index has a document for, read from slot 0
// through one read-only open rather than by walking terms (#2026).
func (u *userIndex) DocGUIDs() ([][16]byte, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	if st.pending > 0 {
		return nil, fmt.Errorf("fts/flatcurve: a batch is in flight; list after it commits")
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return nil, err
	}
	db, err := xapian.OpenDBMulti(paths)
	if err != nil || db == nil {
		return nil, err
	}
	defer db.Close()
	q, err := xapian.QueryMatchAll()
	if err != nil {
		return nil, err
	}
	defer q.Free()
	hits, err := db.SearchWithValue(q, slotGUID)
	if err != nil {
		return nil, err
	}
	seen := make(map[[16]byte]struct{}, len(hits))
	out := make([][16]byte, 0, len(hits))
	for _, h := range hits {
		if len(h.Value) != 16 {
			continue
		}
		var g [16]byte
		copy(g[:], h.Value)
		if _, ok := seen[g]; ok {
			continue
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	return out, nil
}

// DropDocuments removes the documents of messages the store no longer has.
func (u *userIndex) DropDocuments(guids [][16]byte) (int, error) {
	if len(guids) == 0 {
		return 0, nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	if err := st.closeCurrent(); err != nil {
		return 0, err
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return 0, err
	}
	dropped := 0
	for _, p := range paths {
		w, oerr := xapian.OpenWDB(p)
		if oerr != nil {
			return dropped, oerr
		}
		changed := false
		for _, g := range guids {
			ids, derr := w.DocIDsByTerm(guidTerm(g))
			if derr != nil {
				w.Close()
				return dropped, derr
			}
			for _, id := range ids {
				if _, xerr := w.DeleteDocument(id); xerr != nil {
					w.Close()
					return dropped, xerr
				}
				dropped++
				changed = true
			}
		}
		if changed {
			if cerr := w.Commit(); cerr != nil {
				w.Close()
				return dropped, cerr
			}
		}
		w.Close()
	}
	return dropped, nil
}

func (u *userIndex) dropFolders(drop func(folderGUID string) bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	if err := st.closeCurrent(); err != nil {
		return err
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := dropFoldersInShard(p, drop); err != nil {
			return err
		}
	}
	return nil
}

func dropFoldersInShard(path string, drop func(folderGUID string) bool) error {
	// The ids come from a read-only open: a writer has no enumeration of its
	// own, and this walk visits every document, not one term's postings.
	db, err := xapian.OpenDBMulti([]string{path})
	if err != nil || db == nil {
		return err
	}
	ids, err := db.DocIDs()
	db.Close()
	if err != nil {
		return err
	}
	w, err := xapian.OpenWDB(path)
	if err != nil {
		return err
	}
	defer w.Close()
	changed := false
	for _, id := range ids {
		folders, terr := w.DocTerms(id, termFolder)
		if terr != nil {
			return terr
		}
		for _, t := range folders {
			guid := strings.TrimPrefix(t, termFolder)
			if !drop(guid) {
				continue
			}
			if derr := dropFolderFromDoc(w, id, guid); derr != nil {
				return derr
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return w.Commit()
}

// DocCount opens the shards read-only, so an operator count never takes the
// write handle a writer wants (#2017).
func (u *userIndex) DocCount() (uint64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	// A read never commits: the documents of an update in flight belong to it
	// until it says so, and committing them here keeps half its batch (#2021).
	if st.pending > 0 {
		return 0, fmt.Errorf("fts/flatcurve: a batch is in flight; count after it commits")
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return 0, err
	}
	db, err := xapian.OpenDBMulti(paths)
	if err != nil || db == nil {
		return 0, err
	}
	defer db.Close()
	ids, err := db.DocIDs()
	if err != nil {
		return 0, err
	}
	return uint64(len(ids)), nil
}

func (u *userIndex) Rescan(mbox fts.MailboxRef, present []fts.Copy) ([]uint32, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	if err := st.closeCurrent(); err != nil {
		return nil, err
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return nil, err
	}
	live := make(map[[16]byte]uint32, len(present))
	for _, c := range present {
		if c.GUID != ([16]byte{}) {
			live[c.GUID] = c.UID
		}
	}
	indexed := make(map[[16]byte]struct{}, len(live))
	for _, p := range paths {
		if err := reconcileShard(p, mbox.GUID, live, indexed); err != nil {
			return nil, err
		}
	}
	var missing []uint32
	for _, c := range present {
		if _, ok := indexed[c.GUID]; ok {
			continue
		}
		missing = append(missing, c.UID)
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	return missing, nil
}

// reconcileShard reconciles one shard against the folder's live set, by the
// message: a docid is the database's number and names no uid (#2019).
func reconcileShard(path, folderGUID string, live map[[16]byte]uint32, indexed map[[16]byte]struct{}) error {
	w, err := xapian.OpenWDB(path)
	if err != nil {
		return err
	}
	defer w.Close()
	ids, err := w.DocIDsByTerm(folderTerm(folderGUID))
	if err != nil {
		return err
	}
	changed := false
	for _, id := range ids {
		guids, terr := w.DocTerms(id, termGUID)
		if terr != nil {
			return terr
		}
		if len(guids) == 0 {
			continue
		}
		guid, perr := guidOfTerm(guids[0])
		if perr != nil {
			continue
		}
		if _, alive := live[guid]; alive {
			indexed[guid] = struct{}{}
			continue
		}
		if derr := dropFolderFromDoc(w, id, folderGUID); derr != nil {
			return derr
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return w.Commit()
}

// dropFolderFromDoc takes one folder off a document: the terms that name its
// copies go with it, and the document goes when no folder is left.
func dropFolderFromDoc(w *xapian.WDB, id uint32, folderGUID string) error {
	doc, err := w.GetDocument(id)
	if err != nil {
		return err
	}
	defer doc.Free()
	if rerr := doc.RemoveTerm(folderTerm(folderGUID)); rerr != nil {
		return rerr
	}
	left, lerr := w.DocTerms(id, termFolder)
	if lerr != nil {
		return lerr
	}
	if len(left) <= 1 {
		_, derr := w.DeleteDocument(id)
		return derr
	}
	return w.ReplaceDocument(id, doc)
}

// guidOfTerm reads a message GUID back out of its term.
func guidOfTerm(term string) ([16]byte, error) {
	var out [16]byte
	raw, err := hex.DecodeString(strings.TrimPrefix(term, termGUID))
	if err != nil || len(raw) != len(out) {
		return out, fmt.Errorf("fts/flatcurve: bad guid term %q", term)
	}
	copy(out[:], raw)
	return out, nil
}

// Mailboxes is the one index this user has: compaction is per user now, and
// the caller's loop over this list holds the user's lock once (#1986).
func (u *userIndex) Mailboxes() []fts.MailboxRef {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.box == nil {
		return nil
	}
	return []fts.MailboxRef{{}}
}

// OptimizeMailbox compacts sealed shards for one mailbox. It takes the same
// u.mu as Optimize and every write path, so it never runs concurrently with a
// whole-user Optimize or another OptimizeMailbox against the same userIndex.
// optimizeDir is a no-op below 2 shards, so a redundant call (manual and
// auto-optimize racing for the same mailbox) costs nothing.
func (u *userIndex) OptimizeMailbox(mbox fts.MailboxRef) error {
	return u.OptimizeUnderLock(mbox, func(fn func() error) error { return fn() })
}

// OptimizeUnderLock merges outside the lock and takes it only for the switch,
// so a compaction does not shut deliveries out for the length of the merge.
func (u *userIndex) OptimizeUnderLock(_ fts.MailboxRef, withLock func(func() error) error) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	merged, paths, err := mergeShards(st)
	if err != nil || merged == "" {
		return err
	}
	return withLock(func() error { return switchToMerged(st, merged, paths) })
}

// optimizeDir merges a mailbox's sealed shards into one and deletes the
// originals.
//
// NFS INVARIANT -- do not break it for a reader cache. This deletes files
// other code may want to read, and it is safe only because NO shard is held
// open across calls: Lookup, Expunge and Rescan open and close within the
// call, the one long-lived handle (the current write shard) is closed on the
// line below, and every one of those paths takes the same u.mu this runs
// under. Pool open Xapian readers to speed up search and the deletions below
// become unlink-under-open: on NFS that is a client-side silly-rename
// (.nfsXXXX), a directory that will not rmdir, and ESTALE for the reader --
// the reference implementation's failure mode, which we do not have (#1176).
// The cross-process half is the caller's: every compaction runs under that
// mailbox's own FTS lock, never a user-wide one.
// mergeShards writes the merged index beside the shards; an empty path back
// means there was nothing to merge.
func mergeShards(st *mboxState) (string, []string, error) {
	if err := st.closeCurrent(); err != nil {
		return "", nil, err
	}
	paths, err := shardPaths(st.dir)
	if err != nil || len(paths) < 2 {
		return "", nil, err
	}
	db, err := xapian.OpenDBMulti(paths)
	if err != nil {
		return "", nil, err
	}
	tmp := filepath.Join(st.dir, "optimize")
	_ = os.RemoveAll(tmp)
	// Renumbered: every shard numbers from one, and a document is recognised
	// by its terms rather than by its id (#1986).
	cerr := db.CompactRenumbered(tmp)
	db.Close()
	if cerr != nil {
		_ = os.RemoveAll(tmp)
		return "", nil, cerr
	}
	// A message indexed in one shard and copied in another has a document in
	// each: the merge brings them together, and this makes them one.
	if derr := dedupByGUID(tmp); derr != nil {
		_ = os.RemoveAll(tmp)
		return "", nil, derr
	}
	return tmp, paths, nil
}

// switchToMerged puts the merged index in place of the shards it was built
// from. This is the part that must not run beside another writer.
func switchToMerged(st *mboxState, merged string, paths []string) error {
	t0 := time.Now()
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("fts/flatcurve: optimize cleanup: %w", err)
		}
	}
	sealed := filepath.Join(st.dir, fmt.Sprintf("%s%d", dbPrefix, time.Now().UnixMicro()))
	if err := os.Rename(merged, sealed); err != nil {
		return fmt.Errorf("fts/flatcurve: optimize rename: %w", err)
	}
	// Durability of the directory entries themselves, where the medium makes
	// it worth issuing (#1176). Best-effort: a failure costs a rebuild.
	if serr := st.eng.opts.Store.Sync(st.dir); serr != nil {
		slog.Debug("fts/flatcurve: optimize dir sync failed", "dir", st.dir, "err", serr)
	}
	metricOptimizeRuns.Inc()
	metricOptimizeShardsMerged.Add(float64(len(paths)))
	slog.Info("fts/flatcurve: optimize completed", "user", st.user.Username,
		"shards_merged", len(paths), "dur_ms", time.Since(t0).Milliseconds())
	return nil
}

func (u *userIndex) Refresh() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.box == nil {
		return nil
	}
	return u.box.commitCurrent()
}

func (u *userIndex) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.box == nil {
		return nil
	}
	err := u.box.closeCurrent()
	u.box = nil
	return err
}

/* --- lookup -------------------------------------------------------------------- */

// filterToFolders ANDs the folders' boolean terms, ORed together, onto the
// text query.
func filterToFolders(xq *xapian.Query, folders []string) (*xapian.Query, error) {
	var any *xapian.Query
	for _, f := range folders {
		ft, err := xapian.QueryTerm(folderTerm(f))
		if err != nil {
			if any != nil {
				any.Free()
			}
			return nil, err
		}
		if any == nil {
			any = ft
			continue
		}
		joined, jerr := xapian.QueryCombine(xapian.OpOR, any, ft)
		any.Free()
		ft.Free()
		if jerr != nil {
			return nil, jerr
		}
		any = joined
	}
	defer any.Free()
	return xapian.QueryCombine(xapian.OpAND, xq, any)
}

func (u *userIndex) Lookup(folders []string, q fts.Query) (fts.Result, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.state()
	if err := st.commitCurrent(); err != nil {
		return fts.Result{}, err
	}
	paths, err := shardPaths(st.dir)
	if err != nil {
		return fts.Result{}, err
	}
	if len(paths) == 0 {
		return fts.Result{}, nil
	}

	xq, maybe, err := u.buildQuery(q)
	if err != nil {
		return fts.Result{}, err
	}
	defer xq.Free()
	// Folders are a filter, not a database: an empty list searches the whole
	// account, several are ORed together (#1986).
	if len(folders) > 0 {
		filtered, ferr := filterToFolders(xq, folders)
		if ferr != nil {
			return fts.Result{}, ferr
		}
		defer filtered.Free()
		xq = filtered
	}

	// Shard by shard: a database combined from several renumbers what it
	// reports, so hits are merged by the message GUID instead
	// (keeping the higher weight) to stay correct even if that stops holding.
	// Keyed by the message, not by the docid: one message can hold a document
	// in more than one shard, and the shards do not share a numbering.
	best := make(map[[16]byte]float64)
	for _, p := range paths {
		db, derr := xapian.OpenDBMulti([]string{p})
		if derr != nil {
			return fts.Result{}, derr
		}
		entries, serr := db.SearchWithValue(xq, slotGUID)
		slog.Debug("fts/flatcurve: lookup shard", "path", p, "hits", len(entries), "st", fmt.Sprintf("%p", st))
		db.Close()
		if serr != nil {
			return fts.Result{}, serr
		}
		for _, ent := range entries {
			if len(ent.Value) != 16 {
				continue
			}
			var guid [16]byte
			copy(guid[:], ent.Value)
			if w, ok := best[guid]; !ok || ent.Weight > w {
				best[guid] = ent.Weight
			}
		}
	}

	guids := make([][16]byte, 0, len(best))
	for guid := range best {
		guids = append(guids, guid)
	}
	sort.Slice(guids, func(i, j int) bool {
		return bytes.Compare(guids[i][:], guids[j][:]) < 0
	})
	res := fts.Result{}
	for _, guid := range guids {
		if maybe {
			res.MaybeGUIDs = append(res.MaybeGUIDs, guid)
		} else {
			res.DefiniteGUIDs = append(res.DefiniteGUIDs, guid)
		}
	}
	return res, nil
}

func (u *userIndex) buildQuery(q fts.Query) (*xapian.Query, bool, error) {
	if len(q.Terms) == 0 {
		return nil, false, fmt.Errorf("fts/flatcurve: empty query")
	}
	var acc *xapian.Query
	maybe := false
	op := xapian.OpOR
	if q.AndTerms {
		op = xapian.OpAND
	}
	for _, term := range q.Terms {
		tq, tMaybe, err := u.buildTerm(term)
		if err != nil {
			acc.Free()
			return nil, false, err
		}
		// One over-approximated arg makes the whole conjunction a maybe.
		maybe = maybe || tMaybe
		if term.Not {
			all, aerr := xapian.QueryMatchAll()
			if aerr != nil {
				tq.Free()
				acc.Free()
				return nil, false, aerr
			}
			if tq, err = xapian.QueryCombine(xapian.OpANDNOT, all, tq); err != nil {
				acc.Free()
				return nil, false, err
			}
		}
		if acc == nil {
			acc = tq
		} else if acc, err = xapian.QueryCombine(op, acc, tq); err != nil {
			return nil, false, err
		}
	}
	return acc, maybe, nil
}

func (u *userIndex) buildTerm(t fts.Term) (*xapian.Query, bool, error) {
	name := strings.ToLower(t.HdrName)
	if len(t.Words) == 0 {
		if t.Field != fts.FieldHeader {
			return nil, false, fmt.Errorf("fts/flatcurve: empty term")
		}
		// HEADER existence probe → the boolean term.
		q, err := xapian.QueryTerm(boolPrefix + name)
		return q, false, err
	}
	prefix := u.eng.prefix
	var acc *xapian.Query
	maybe := false
	for _, w := range t.Words {
		var wq *xapian.Query
		for _, v := range w.Variants {
			v = normTerm(v, 1)
			if v == "" {
				continue
			}
			vq, vMaybe, err := buildVariant(t.Field, name, v, prefix)
			if err != nil {
				wq.Free()
				acc.Free()
				return nil, false, err
			}
			maybe = maybe || vMaybe
			if wq == nil {
				wq = vq
			} else if wq, err = xapian.QueryCombine(xapian.OpOR, wq, vq); err != nil {
				acc.Free()
				return nil, false, err
			}
		}
		if wq == nil {
			continue
		}
		var err error
		if acc == nil {
			acc = wq
		} else if acc, err = xapian.QueryCombine(xapian.OpAND, acc, wq); err != nil {
			return nil, false, err
		}
	}
	if acc == nil {
		return nil, false, fmt.Errorf("fts/flatcurve: no usable variants in term")
	}
	return acc, maybe, nil
}

// buildVariant turns one search word into a query.
//
// Whether a term is expanded as a prefix is the caller's setting, not a
// property of the field. Expanding unconditionally is the most expensive of the
// available behaviours and was reached by omission rather than by choice: this
// function received the term-size threshold and never read it (#1052).
func buildVariant(field fts.FieldKind, hdrName, v string, prefix PrefixRange) (*xapian.Query, bool, error) {
	term := func(s string) (*xapian.Query, error) {
		if prefix.Allows(v) {
			return xapian.QueryWildcard(s)
		}
		return xapian.QueryTerm(s)
	}
	switch field {
	case fts.FieldBody:
		q, err := term(v)
		return q, false, err
	case fts.FieldText:
		hq, err := term(allHdrPrefix + v)
		if err != nil {
			return nil, false, err
		}
		bq, err := term(v)
		if err != nil {
			hq.Free()
			return nil, false, err
		}
		q, err := xapian.QueryCombine(xapian.OpOR, hq, bq)
		return q, false, err
	case fts.FieldHeader:
		if indexedHeaders[hdrName] {
			q, err := term(hdrPrefix + strings.ToUpper(hdrName) + v)
			return q, false, err
		}
		// Non-indexed header: only the pooled A prefix knows the term, an
		// over-approximation the caller must re-verify (maybe).
		q, err := term(allHdrPrefix + v)
		return q, true, err
	default:
		return nil, false, fmt.Errorf("fts/flatcurve: unknown field kind %d", field)
	}
}

// openShard opens one shard for reading its terms back.
func openShard(path string) (*xapian.WDB, error) { return xapian.OpenWDB(path) }

// dedupByGUID folds the documents of one message into the first of them: the
// copies' terms join it, and the rest go.
func dedupByGUID(dir string) error {
	w, err := xapian.OpenWDB(dir)
	if err != nil {
		return err
	}
	defer w.Close()
	last, err := w.LastDocID()
	if err != nil {
		return err
	}
	seen := make(map[string]uint32, last)
	for id := uint32(1); id <= last; id++ {
		guids, terr := w.DocTerms(id, termGUID)
		if terr != nil || len(guids) == 0 {
			continue
		}
		kept, ok := seen[guids[0]]
		if !ok {
			seen[guids[0]] = id
			continue
		}
		if err := mergeCopies(w, kept, id); err != nil {
			return err
		}
		metricDedupMerged.Inc()
	}
	return w.Commit()
}

// mergeCopies moves one document's copy terms onto the document that stays.
func mergeCopies(w *xapian.WDB, keep, drop uint32) error {
	into, err := w.GetDocument(keep)
	if err != nil {
		return err
	}
	defer into.Free()
	terms, terr := w.DocTerms(drop, termFolder)
	if terr != nil {
		return terr
	}
	for _, t := range terms {
		if err := into.AddBooleanTerm(t); err != nil {
			return err
		}
	}
	if err := w.ReplaceDocument(keep, into); err != nil {
		return err
	}
	_, derr := w.DeleteDocument(drop)
	return derr
}
