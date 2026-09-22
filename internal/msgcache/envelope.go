// Package msgcache reads and writes the per-folder index cache
// (yarilo.index.cache) that lets a listing answer without opening message
// files. Shared by the protocol servers: one cache, one set of invalidation
// rules. Format and invalidation: https://doc.yarilomail.org/BACKEND-API
package msgcache

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/imaptext"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// cachePathMu serialises cache-pair access within this process, keyed by the
// cache file path -- the in-process fast path of the two-tier rule; the
// cross-process tier is the MailboxKey lock below. Entries are never removed:
// the set of open folders per process is small and bounded.
var cachePathMu sync.Map // path -> *sync.Mutex

// shared takes the read side: without it, sharing the cross-process key buys
// nothing for two sessions in one pod (#1673).
func lockCachePath(path string, shared bool) func() {
	mu, _ := cachePathMu.LoadOrStore(path, &sync.RWMutex{})
	m, ok := mu.(*sync.RWMutex)
	if !ok {
		// Unreachable: this map only ever stores *sync.RWMutex. Failing open
		// would silently drop the in-process tier, so fail loud instead.
		panic("imap: cache mutex map holds a foreign type")
	}
	if shared {
		m.RLock()
		return m.RUnlock
	}
	m.Lock()
	return m.Unlock
}

// Index is the slice of the index surface the cache needs. Asserted at use:
// a backend without it serves no cache.
type Index interface {
	CachePairIdentity(folderID uint64) (indexID, resetID uint32, ok bool, err error)
	// EnsureCacheExtension adds the extension to an index written before it
	// existed. Without it the lazy add is unreachable: only a stamping write
	// adds the extension, and stamping needs a pair that the missing
	// extension prevents opening.
	EnsureCacheExtension(folderID uint64) (indexID, resetID uint32, err error)
	// BumpCacheGeneration abandons the current generation and returns the
	// next file_seq. Discarding a file without it leaves the index's stamps
	// applying to whatever gets written at those offsets next.
	BumpCacheGeneration(folderID uint64) (uint32, error)
	CachePath(folderID uint64) (string, error)
	SetCacheOffsets(folderID uint64, stamps map[uint32]mailbox.CacheStamp) error
}

/* --- per-FETCH cache handle ----------------------------------------------- */

// Handle is one request's view of a folder's cache, closed after the batched
// stamp. nil means "no cache": every method tolerates it (#1176).
type Handle struct {
	file   *mailindex.CacheFile
	ids    map[string]uint32             // reference field name -> id in this file
	stamps map[uint32]mailbox.CacheStamp // uid -> head offset and checksum, flushed on close
	// merged is what this handle has written for a message, so the checksum it
	// stamps covers the whole record rather than the last field appended.
	merged map[uint32]map[uint32][]byte
	crcs   map[uint32]uint32
	idx    Index
	fid    uint64
	// unlock releases the locks taken for the open-append-stamp window, in
	// reverse acquisition order. Two sessions of one account in one folder
	// are the ordinary case: without the lock their appends interleave under
	// two descriptors, and a stamped offset resolves inside a file the other
	// writer has since extended -- a fully valid-looking record for a
	// DIFFERENT message, which no invalidation level can see.
	unlock []func()

	// Deferred mode: what the request parsed, kept until Close can take the
	// locks again. Order matters -- two fields for one message chain, so they
	// must be appended in the order they were produced.
	deferred bool
	pending  []pendingField
	// reopen carries what the second window needs to prove it is looking at
	// the same cache generation the first one read.
	reopen struct {
		idx     mailbox.UserIndex
		ic      Index
		fid     uint64
		opts    Options
		path    string
		indexID uint32
		resetID uint32
	}
}

// pendingField is one field value a deferred handle has not written yet.
type pendingField struct {
	meta    mailbox.MessageMeta
	fieldID uint32
	data    []byte
}

// UID is the message this field belongs to.
func (p pendingField) UID() uint32 { return p.meta.UID }

// Options carries the lock identity and a trace id.
// lockID: a caller that supplied none still names a holder (#1670).
func (o Options) lockID() string {
	if o.SessionID != "" {
		return o.SessionID
	}
	if o.TraceID != "" {
		return o.TraceID
	}
	return locks.NewID()
}

type Options struct {
	Locker locks.Locker
	User   string
	// SessionID names the session in the lock owner. Required: a cache write
	// holds the mailbox key, and an anonymous holder is unattributable (#1670).
	SessionID string
	Folder    string
	TraceID   string
	// DeferWrites releases the cache locks as soon as the file has been read
	// into memory, and takes them again in Close to append what the request
	// parsed.
	//
	// For a caller that writes its response while holding the handle. FETCH
	// does: it opened the pair, then read bodies from storage and wrote them
	// to a socket, all inside the locked window, so one client on a slow link
	// held both tiers -- the in-process path mutex and the cross-process
	// mailbox lock -- for as long as its transfer took. Every other session of
	// that user on that folder waited (#1545).
	DeferWrites bool

	// Shared: readers of one folder do not refuse each other. Requires
	// DeferWrites, and never for a caller that changes flags (#1673).
	Shared bool
}

// openExclusive retries a shared open that reached a step which writes. Once:
// the second pass is not shared, so it cannot come back here.
func openExclusive(idx mailbox.UserIndex, folderID uint64, opts Options) *Handle {
	opts.Shared = false
	return Open(idx, folderID, opts)
}

// Open returns a handle on the folder's cache, or nil when none can be
// served -- a wrong pair, an unopenable file, a backend without a cache. The
// caller parses as it would without one.
func Open(idx mailbox.UserIndex, folderID uint64, opts Options) *Handle {
	if opts.Shared && !opts.DeferWrites {
		// storeField writes through a live descriptor when the handle is not
		// deferred, and a shared key does not exclude the other writer (#1673).
		if flag.Lookup("test.v") != nil {
			panic("msgcache: Shared without DeferWrites writes the cache under a shared key (#1673)")
		}
		slog.Error("msgcache: Shared without DeferWrites; opening exclusively")
		opts.Shared = false
	}
	ic, ok := idx.(Index)
	if !ok {
		return nil
	}
	indexID, resetID, extOK, err := ic.CachePairIdentity(folderID)
	if err != nil {
		return nil
	}
	if !extOK {
		// Every mailbox older than the extension arrives here, which is every
		// mailbox in an upgraded deployment.
		if indexID, resetID, err = ic.EnsureCacheExtension(folderID); err != nil {
			slog.Debug("msgcache: cache extension unavailable; serving uncached", "trace_id", opts.TraceID, "err", err)
			return nil
		}
	}
	path, err := ic.CachePath(folderID)
	if err != nil {
		return nil
	}
	fc := &Handle{idx: ic, fid: folderID, stamps: make(map[uint32]mailbox.CacheStamp)}
	// The whole open-append-stamp window runs under the lock, reads
	// included: remove-and-recreate under a live descriptor and the
	// read-modify-write of the field table are only safe when nobody else
	// is inside the pair. In-process mutex first, then the cross-process
	// MailboxKey -- the same two tiers every shared write path uses.
	fc.unlock = append(fc.unlock, lockCachePath(path, opts.Shared))
	if lkr := opts.Locker; lkr != nil && opts.User != "" && opts.Folder != "" {
		ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), "msgcache"), 35*time.Second)
		key := locks.MailboxKey(opts.User, opts.Folder)
		owner := locks.Owner(opts.User, opts.lockID())
		var lk locks.Lock
		var lerr error
		if opts.Shared {
			lk, lerr = locks.AcquireShared(ctx, lkr, key, owner, 30*time.Second)
		} else {
			lk, lerr = locks.Acquire(ctx, lkr, key, owner, 30*time.Second)
		}
		cancel()
		if lerr != nil {
			slog.Debug("msgcache: cache lock unavailable; serving uncached", "trace_id", opts.TraceID, "err", lerr)
			fc.release()
			return nil
		}
		fc.unlock = append(fc.unlock, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = lkr.Unlock(ctx, lk.ID)
		})
	}
	cf, err := mailindex.OpenCache(path, indexID, resetID)
	switch {
	case err == nil:
		fc.file = cf
	case os.IsNotExist(err), errors.Is(err, mailindex.ErrCacheInvalid):
		if opts.Shared {
			// Creating or repairing the file is a write, and this handle holds
			// the key in shared mode. Start again exclusively (#1673).
			fc.release()
			return openExclusive(idx, folderID, opts)
		}
		if errors.Is(err, mailindex.ErrCacheInvalid) {
			// Garbage by definition -- but recreating under the SAME file_seq
			// would leave the index's stamps pointing into a fresh file,
			// where the first append to reach one of those offsets answers
			// its FETCH with another message's record. Enter a new
			// generation, which kills every stamp in one index write.
			_ = os.Remove(path)
			newSeq, berr := ic.BumpCacheGeneration(folderID)
			if berr != nil {
				slog.Debug("msgcache: cache generation bump failed; serving uncached", "trace_id", opts.TraceID, "err", berr)
				fc.release()
				return nil
			}
			resetID = newSeq
		}
		cf, cerr := mailindex.CreateCache(path, indexID, resetID)
		if cerr != nil {
			slog.Debug("msgcache: cache create failed; serving uncached", "trace_id", opts.TraceID, "err", cerr)
			fc.release()
			return nil
		}
		fc.file = cf
	default:
		slog.Debug("msgcache: cache open failed; serving uncached", "trace_id", opts.TraceID, "err", err)
		fc.release()
		return nil
	}
	// The whole table is registered up front: AddFields is a read-modify-write
	// of the in-file table, so once per window beats once per field.
	fc.ids = make(map[string]uint32, len(referenceFields))
	for _, want := range referenceFields {
		id, ok := fc.file.FieldID(want.Name)
		if !ok {
			if opts.Shared {
				// AddFields is a read-modify-write of the in-file table.
				fc.file.Close()
				fc.release()
				return openExclusive(idx, folderID, opts)
			}
			first, aerr := fc.file.AddFields([]mailindex.CacheField{want})
			if aerr != nil {
				fc.file.Close()
				fc.release()
				return nil
			}
			id = first
		}
		fc.ids[strings.ToLower(want.Name)] = id
	}
	fc.reopen.idx, fc.reopen.ic = idx, ic
	fc.reopen.fid, fc.reopen.opts = folderID, opts
	fc.reopen.path, fc.reopen.indexID, fc.reopen.resetID = path, indexID, resetID
	if opts.DeferWrites {
		// Reads come from memory from here on, so the locks are not protecting
		// anything the caller still does. What they do protect -- appending
		// under a second descriptor, and remove-and-recreate -- happens in
		// Close, under a fresh window.
		//
		// A snapshot going stale is a cache miss and nothing worse: another
		// session's append is simply not in it, and the message is re-parsed.
		fc.file.Preload()
		fc.deferred = true
		fc.release()
	}
	return fc
}

// flush writes what a deferred handle collected, under a second window.
//
// The generation is re-checked rather than assumed. Between the two windows
// another session may have found the file invalid and bumped it, and appending
// into a new generation at offsets computed against the old one would stamp the
// index at positions where some other message's record will land -- a valid
// record for the wrong message, which is the one failure no invalidation level
// can see. A changed generation drops the writes: they are derived data, and
// the next read re-parses.
func (fc *Handle) flush() {
	if len(fc.pending) == 0 {
		return
	}
	r := fc.reopen
	// The session travels: this window is the exclusive one every shared FETCH
	// now pays, and held_by must name who holds it (#1670).
	second := Open(r.idx, r.fid, Options{
		Locker: r.opts.Locker, User: r.opts.User, Folder: r.opts.Folder,
		TraceID: r.opts.TraceID, SessionID: r.opts.SessionID,
	})
	if second == nil {
		return
	}
	// Checked here and not before opening: the identity read outside the lock
	// is stale the moment it is read, and a bump landing between that read and
	// this Open would put these records into a generation they were not
	// computed against. second read its own identity while holding both locks,
	// so this comparison is the one that decides.
	if second.reopen.indexID != r.indexID || second.reopen.resetID != r.resetID {
		slog.Debug("msgcache: cache generation moved while the response was written; dropping cached fields",
			"trace_id", r.opts.TraceID, "pending", len(fc.pending))
		second.Close()
		return
	}

	// Chain heads are re-read, not carried over. Between the two windows
	// another session may have appended a record for one of these UIDs and
	// stamped a new head; chaining from the offset this request saw would hop
	// over that record and leave it unreachable. Losing a cached field is only
	// a re-parse, but it is exactly the "tolerate what somebody else wrote"
	// that splitting the window promised (#1545).
	heads := map[uint32]mailbox.CacheStamp{}
	if msgs, merr := r.idx.GetMessages(r.fid, mailbox.SeqSet{}); merr == nil {
		for _, m := range msgs {
			heads[m.UID] = mailbox.CacheStamp{Offset: m.CacheOffset, CRC: m.CacheCRC}
		}
	} else {
		slog.Debug("msgcache: could not re-read chain heads; dropping cached fields",
			"trace_id", r.opts.TraceID, "err", merr)
		second.Close()
		return
	}
	for _, p := range fc.pending {
		stamp, live := heads[p.UID()]
		if !live {
			// Expunged while the response was being written, which is ordinary
			// on a busy folder. Appending anyway writes bytes no chain reaches:
			// nothing stamps an offset for a UID the index no longer carries,
			// so the record sits in the file until the generation is bumped.
			// Not corruption -- growth on the path a busy mailbox takes most
			// often (#1549).
			continue
		}
		// The checksum travels with the offset: a stale one makes every later
		// read of that record a mismatch.
		meta := p.meta
		meta.CacheOffset, meta.CacheCRC = stamp.Offset, stamp.CRC
		second.storeField(&meta, p.fieldID, p.data)
	}
	second.Close()
}

// head is the message's current chain head: the offset stamped earlier in
// THIS window when there is one, else what the index carries. Without it a
// second append for the same message in one FETCH (envelope, then body
// structure) would chain from the stale head and orphan the first value.
func (fc *Handle) head(m *mailbox.MessageMeta) uint32 {
	if fc == nil {
		return 0
	}
	if stamp, ok := fc.stamps[m.UID]; ok {
		return stamp.Offset
	}
	return m.CacheOffset
}

// read returns the merged field values for a message, or nil on a miss. What
// it read is kept: a store follows a miss, and reading the chain a second time
// to checksum it cost a fifth of the backend's CPU (#1714).
func (fc *Handle) read(m *mailbox.MessageMeta) map[uint32][]byte {
	if fc == nil {
		return nil
	}
	if vals, ok := fc.chain(m.UID); ok {
		return vals
	}
	off := fc.head(m)
	if off == 0 {
		fc.keepChain(m.UID, nil) // nothing cached, and nothing to re-read for
		return nil
	}
	vals, err := fc.file.ReadRecord(off)
	if err != nil {
		fc.startFreshChain(m) // a bad chain is a miss; nothing of it is kept
		return nil
	}
	// The checksum before the fields (cyrus mailbox.c:705-775): a record that
	// hashes to something else is another message's, field by field.
	if crc := fc.recordCRCFor(m); crc != 0 && mailindex.RecordCRC(vals) != crc {
		metricCRCMismatch.Inc()
		slog.Debug("msgcache: cache record checksum mismatch; re-reading the message", "uid", m.UID)
		fc.startFreshChain(m)
		return nil
	}
	fc.keepChain(m.UID, vals)
	return vals
}

// startFreshChain drops a record this handle would not serve: the next append
// must not hang off it, or the checksum we stamp covers less than a reader
// merges and every later read is a mismatch (#1714).
func (fc *Handle) startFreshChain(m *mailbox.MessageMeta) {
	fc.keepChain(m.UID, nil)
	fc.stamps[m.UID] = mailbox.CacheStamp{}
}

// chain is what this handle has read or written for a message; the bool says
// whether the chain is known at all, so an empty one is not a second read.
func (fc *Handle) chain(uid uint32) (map[uint32][]byte, bool) {
	if fc.merged == nil {
		return nil, false
	}
	vals, ok := fc.merged[uid]
	return vals, ok
}

// keepChain records what a message's record holds, copied: the values belong
// to the reader that produced them.
func (fc *Handle) keepChain(uid uint32, vals map[uint32][]byte) {
	if fc.merged == nil {
		fc.merged = make(map[uint32]map[uint32][]byte)
		fc.crcs = make(map[uint32]uint32)
	}
	kept := make(map[uint32][]byte, len(vals)+len(referenceFields))
	for id, v := range vals {
		kept[id] = v
	}
	fc.merged[uid] = kept
}

// recordCRCFor is the checksum the index holds for a message, or zero when the
// index carries none: one written by the reference, read at its bounds.
func (fc *Handle) recordCRCFor(m *mailbox.MessageMeta) uint32 {
	if crc, ok := fc.crcs[m.UID]; ok {
		return crc
	}
	return m.CacheCRC
}

// storeField appends one field value for a message and moves the chain head.
func (fc *Handle) storeField(m *mailbox.MessageMeta, fieldID uint32, data []byte) {
	if fc == nil {
		return
	}
	if fc.deferred {
		// Copied: the caller's buffer belongs to the response being written.
		buf := make([]byte, len(data))
		copy(buf, data)
		fc.pending = append(fc.pending, pendingField{meta: *m, fieldID: fieldID, data: buf})
		return
	}
	off, err := fc.file.AppendRecord(fc.head(m), []mailindex.CacheFieldValue{
		{FieldID: fieldID, Data: data},
	})
	if err != nil {
		slog.Debug("msgcache: cache append failed", "uid", m.UID, "err", err)
		return
	}
	fc.remember(m, fieldID, data)
	fc.stamps[m.UID] = mailbox.CacheStamp{Offset: off, CRC: mailindex.RecordCRC(fc.merged[m.UID])}
}

// remember adds a field to what the handle knows the record holds and
// checksums it from memory, as the reference's neighbour does over the buffer
// it just wrote (cyrus message.c:2170).
func (fc *Handle) remember(m *mailbox.MessageMeta, fieldID uint32, data []byte) {
	vals, ok := fc.chain(m.UID)
	if !ok {
		// A store with no read before it: the chain has to come from disk
		// once, and the counter says how often that happens (#1714).
		metricChainReread.Inc()
		fc.read(m)
		vals, _ = fc.chain(m.UID)
	}
	vals[fieldID] = data
	fc.crcs[m.UID] = mailindex.RecordCRC(vals)
}

// envelope returns the cached envelope for a message, or nil on any of the
// three misses.
func (fc *Handle) Envelope(m *mailbox.MessageMeta) *imaplib.Envelope {
	if fc == nil {
		return nil
	}
	text, ok := fc.EnvelopeText(m)
	if !ok {
		return nil // no record, or a record with neither envelope nor headers
	}
	env, ok := imaptext.ParseEnvelope(text)
	if !ok {
		return nil
	}
	return env
}

// Preload tells the handle it is about to be read in full, so it reads the
// cache file once instead of paying a syscall per record. Callers that touch
// one message should not call it.
func (fc *Handle) Preload() {
	if fc == nil || fc.file == nil {
		return
	}
	fc.file.Preload()
}

// References returns the cached References header as a list of message ids, or
// nil when the message has no record, no such field, or genuinely had no
// References header.
//
// The last two cases are told apart by the empty marker: a message with no
// References is stored as one empty entry, so that a header nobody has to read
// again is not re-read for ever.
func (fc *Handle) References(m *mailbox.MessageMeta) ([]string, bool) {
	if fc == nil {
		return nil, false
	}
	data, ok := fc.read(m)[fc.fieldID(fieldHdrReferences)]
	if !ok {
		return nil, false
	}
	return referencesFromHeader(data)
}

// EnvelopeAndReferences reads both in ONE pass over the message's record.
//
// Asking for them separately costs two full reads -- each walks the record
// chain, does its own ReadAt and decodes every field in it -- and threading
// needs both for every message it touches. On ten thousand messages that
// second pass was most of what a THREAD cost: the algorithm itself is ~8ms
// there, while the doubled read is ~170ms (#1461).
func (fc *Handle) EnvelopeAndReferences(m *mailbox.MessageMeta) (*imaplib.Envelope, []string, bool) {
	if fc == nil {
		return nil, nil, false
	}
	vals := fc.read(m)
	envData, ok := vals[fc.fieldID(fieldIMAPEnvelope)]
	if !ok {
		return nil, nil, false
	}
	env, ok := imaptext.ParseEnvelope(string(envData))
	if !ok {
		return nil, nil, false
	}
	refsData, cached := vals[fc.fieldID(fieldHdrReferences)]
	if !cached {
		return env, nil, false
	}
	refs, ok := referencesFromHeader(refsData)
	return env, refs, ok
}

// StoreReferences caches the References of a message. An empty list is stored
// as an empty value rather than skipped: "no References" is an answer, and
// skipping it would make every such message a permanent miss.
func (fc *Handle) StoreReferences(m *mailbox.MessageMeta, refs []string) {
	if fc == nil {
		return
	}
	fc.storeField(m, fc.fieldID(fieldHdrReferences), encodeReferencesHeader(refs))
}

// StoreEnvelope caches an envelope a caller holds as a struct. The text is the
// stored form, so what a client is shown does not depend on who wrote it.
func (fc *Handle) StoreEnvelope(m *mailbox.MessageMeta, env *imaplib.Envelope) {
	if fc == nil || env == nil {
		return
	}
	fc.StoreEnvelopeText(m, imaptext.WriteEnvelope(env))
}

// StoreEnvelopeText caches the envelope exactly as it will be answered, built
// from the raw header by the reference's rules (#1714).
func (fc *Handle) StoreEnvelopeText(m *mailbox.MessageMeta, text string) {
	if fc == nil || text == "" {
		return
	}
	fc.storeField(m, fc.fieldID(fieldIMAPEnvelope), []byte(text))
}

// EnvelopeText is the stored envelope; a record holding only headers has one
// built from them and written back (index-mail-headers.c:515-560).
func (fc *Handle) EnvelopeText(m *mailbox.MessageMeta) (string, bool) {
	if fc == nil {
		return "", false
	}
	vals := fc.read(m)
	if data, ok := vals[fc.fieldID(fieldIMAPEnvelope)]; ok && len(data) > 0 {
		return string(data), true
	}
	text, ok := fc.envelopeFromCachedHeaders(vals)
	if !ok {
		return "", false
	}
	fc.StoreEnvelopeText(m, text)
	return text, true
}

// bodyStructure returns the cached body structure, or nil on any miss.
func (fc *Handle) BodyStructure(m *mailbox.MessageMeta) imaplib.BodyStructure {
	if fc == nil {
		return nil
	}
	data, ok := fc.read(m)[fc.fieldID(fieldIMAPBodyStructure)]
	if !ok {
		return nil
	}
	bs, ok := imaptext.ParseBodyStructure(string(data))
	if !ok {
		return nil
	}
	return bs
}

// storeBodyStructure appends the freshly-parsed body structure. A structure
// that does not survive its own codec is not stored: serving a value the
// reader would reject is worse than a miss, and this is where an unknown
// node kind is caught.
func (fc *Handle) StoreBodyStructure(m *mailbox.MessageMeta, bs imaplib.BodyStructure) {
	if fc == nil || bs == nil {
		return
	}
	enc, ok := imaptext.WriteBodyStructure(bs, true)
	if !ok {
		slog.Debug("msgcache: body structure not representable; leaving uncached", "uid", m.UID)
		return
	}
	if _, ok := imaptext.ParseBodyStructure(enc); !ok {
		slog.Debug("msgcache: body structure did not survive its own codec; leaving uncached", "uid", m.UID)
		return
	}
	body, ok := imaptext.WriteBodyStructure(bs, false)
	if !ok {
		return
	}
	fc.storeField(m, fc.fieldID(fieldIMAPBodyStructure), []byte(enc))
	fc.storeField(m, fc.fieldID(fieldIMAPBody), []byte(body))
}

// close flushes the batched offset stamps -- one index write per FETCH, not
// per message -- releases the descriptor (no long-lived handles, #1176), and
// only then drops the locks: the stamp is part of the guarded window.
func (fc *Handle) Close() {
	if fc == nil {
		return
	}
	if fc.deferred {
		if fc.file != nil {
			if err := fc.file.Close(); err != nil {
				slog.Debug("msgcache: cache close failed", "err", err)
			}
			fc.file = nil
		}
		fc.flush()
		return
	}
	if len(fc.stamps) > 0 {
		if err := fc.idx.SetCacheOffsets(fc.fid, fc.stamps); err != nil {
			slog.Debug("msgcache: cache stamp failed", "err", err)
		}
	}
	if fc.file != nil {
		if err := fc.file.Close(); err != nil {
			slog.Debug("msgcache: cache close failed", "err", err)
		}
	}
	fc.release()
}

// release drops held locks in reverse acquisition order.
func (fc *Handle) release() {
	for i := len(fc.unlock) - 1; i >= 0; i-- {
		fc.unlock[i]()
	}
	fc.unlock = nil
}
