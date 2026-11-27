// Package threads persists which conversation each message belongs to.
//
// # Why a log and not a rewritten file
//
// The other per-user state files in this tree (subscriptions, special-use)
// are rewritten whole on every change, which is right for a set that changes
// a few times a day and is read far more often than written. This one is
// written on every delivery, and its size grows with the mailbox: rewriting
// it per message would make delivery cost O(account) per message, on the hot
// path, for the largest accounts first.
//
// So it is an append-only log with periodic compaction, the shape the mail
// index already uses for the same reason. A delivery appends a few short
// records under the account's thread lock; a reader loads the file once and
// folds it in memory.
//
// # Records
//
// TAB-delimited, LF-terminated, one record per line:
//
//	M<TAB><message-id><TAB><thread>    a Message-ID belongs to a thread
//	S<TAB><subject-key><TAB><thread>   a normalised subject was last seen on a thread
//	G<TAB><guid><TAB><thread>          one of OUR messages belongs to a thread
//	A<TAB><old-thread><TAB><thread>    old-thread was merged INTO thread
//
// An unknown record type is skipped rather than refused: a file written by a
// newer build must not make an older one refuse to thread at all, and the
// worst a skipped record costs is a join.
//
// # Merges never delete
//
// When a late message joins two conversations they merge, and the merge is an
// A record rather than a rewrite of every G record that named the old thread.
// A thread id therefore never dangles: it resolves, through as many aliases as
// it takes, to the thread that survives. That keeps a merge O(1) on the
// delivery path, and it keeps the id a client cached last week meaningful --
// it now names the merged conversation, which is what RFC 8621 expects a
// client to be told through Thread/changes.
//
// Alias chains are folded at compaction, so they do not grow without bound.
package threads

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/sortedfile"
)

// FileName is the sidecar's name in the user's control directory.
const FileName = "threads"

// Record types on the wire.
const (
	recMessage = "M"
	recSubject = "S"
	recGUID    = "G"
	recAlias   = "A"
	// recNext names a full conversation's continuation: N<TAB>full<TAB>next.
	recNext = "N"
	// recGen heads a compacted file: C<TAB><generation><TAB><unix-seconds>.
	//
	// Compaction rewrites the log, so a position in the old file means nothing
	// in the new one -- and a client's position would still be a valid number,
	// pointing at unrelated records. The generation is what makes that
	// detectable instead of a confident diff of the wrong history.
	//
	// A build that predates this record skips it as an unknown type and counts
	// it like any other, so positions stay comparable in both directions.
	recGen = "C"
)

// State is the folded contents of the log: what the account knows about its
// conversations right now.
//
// # Why the lock is here and not around it
//
// Writers are serialised ACROSS processes by the account's thread lock, which
// is where cross-process consistency belongs. What remains is a race inside
// one process: a reader (Thread/get) walking these maps while a delivery
// applies a placement to them.
//
// The distributed lock is the wrong currency for that -- a reader would pay a
// network round trip to the lock service per request and queue behind LMTP,
// making a JMAP read as slow as the writes it has nothing to do with. A
// snapshot is the other wrong answer: copying the account per request is the
// O(account) cost the fold cache exists to avoid, moved onto the read path.
//
// So: an RWMutex, held for nanoseconds. Readers take it shared, the memory
// half of Append takes it exclusively -- and because Append applies to memory
// only after the file write succeeded, that block is indivisible: a reader
// never sees a message placed without the alias that placed it.
type State struct {
	mu sync.RWMutex
	// records is how many log records this state folded, and therefore the
	// account's threading state string.
	records int
	// generation counts compactions. A position is meaningful only within one.
	generation uint64
	byMessage  map[string]string
	bySubject  map[string]string
	byGUID     map[string]string
	// pair is the base under these maps, which then hold the log alone: a
	// miss is asked of the base by key (THREADS-STORE.md, #2641). Nil for a
	// whole-log fold.
	pair *Store
	// size is each live thread's member count, moved by G records and
	// aliases: the limit a placement keeps (threading_max_thread).
	size    map[string]int
	aliasOf map[string]string
	// next maps a full conversation to the one that continues it.
	next map[string]string
}

// Empty is the state of an account whose sidecar does not exist yet -- which
// is every account until the migration step has run for it. It answers "no
// thread" to everything, so threading degrades to one-message-one-thread
// rather than to an error.
func Empty() *State {
	return &State{
		byMessage: map[string]string{},
		bySubject: map[string]string{},
		byGUID:    map[string]string{},
		size:      map[string]int{},
		aliasOf:   map[string]string{},
		next:      map[string]string{},
	}
}

// Load folds the account whole: the base, when there is one, and the log
// over it. For the readers that still want every map (JMAP, the cache, the
// rebuild); a delivery searches the pair instead (store.go).
func Load(path string) (*State, error) {
	if _, err := os.Stat(path + BaseSuffix); err != nil {
		return loadLog(path)
	}
	// A base moved under the fold (ESTALE, a short read on NFS) is the pair
	// opened again, as a lookup does (THREADS-STORE.md §6).
	for attempt := 1; ; attempt++ {
		st, err := loadPair(path)
		if !errors.Is(err, sortedfile.ErrMoved) || attempt >= pairAttempts {
			return st, err
		}
	}
}

func loadPair(path string) (*State, error) {
	store, err := OpenStore(path)
	if err != nil {
		return nil, err
	}
	// The log's maps, with the base under them for what they miss: the
	// readers ask by key, and nothing of the account is folded.
	st := store.tail
	st.pair = store
	st.records = store.baseRecords() + store.tailRecords
	st.generation = store.base.Generation
	return st, nil
}

// loadLog folds the log at path, the account's whole history when no base
// sits under it. A missing file is not an error: see Empty.
func loadLog(path string) (*State, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Empty(), nil
		}
		return nil, fmt.Errorf("threads: open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	st := Empty()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) != 3 {
			continue // truncated tail or a record shape we do not know
		}
		st.apply(fields[0], fields[1], fields[2])
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("threads: read %s: %w", path, err)
	}
	return st, nil
}

// resolve follows the alias chain to the thread that survives.
//
// Bounded by the number of aliases, and defensively by a step limit: a file
// corrupted into a cycle must not hang a delivery. A cycle answers with the id
// it started from, which is wrong but finite, and compaction breaks it.
func (s *State) resolve(id string) string {
	for i := 0; id != "" && i < len(s.aliasOf)+1; i++ {
		next, ok := s.aliasOf[id]
		if !ok {
			return id
		}
		id = next
	}
	return id
}

// ThreadOfMessage implements threading.Known.
func (s *State) ThreadOfMessage(messageID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byMessage[messageID]
	if !ok {
		return s.fromBase(secMessage, messageID)
	}
	return s.resolve(id), true
}

// fromBase asks the base under a pair's state: a read that fails is a miss,
// and the cache, keyed by the log's stat, folds the pair again once a merge
// has moved it.
func (s *State) fromBase(sec uint32, key string) (string, bool) {
	if s.pair == nil || s.pair.base == nil {
		return "", false
	}
	v, ok, err := s.pair.base.Get(sec, []byte(key))
	if err != nil {
		slog.Debug("threads: base unreadable, answered as unknown", "path", s.pair.path, "err", err)
		return "", false
	}
	if !ok {
		return "", false
	}
	return s.resolve(string(v)), true
}

// ThreadSize is how many messages the conversation holds.
func (s *State) ThreadSize(threadID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.pair != nil {
		n, _ := s.pair.ThreadSize(threadID)
		return n
	}
	return s.size[s.resolve(threadID)]
}

// Continuation is the conversation that continues a full one, or "".
func (s *State) Continuation(threadID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id, ok := s.next[s.resolve(threadID)]; ok {
		return s.resolve(id)
	}
	id, _ := s.fromBase(secNext, s.resolve(threadID))
	return id
}

// ThreadOfSubject implements threading.Known.
func (s *State) ThreadOfSubject(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.bySubject[key]
	if !ok {
		return s.fromBase(secSubject, key)
	}
	return s.resolve(id), true
}

// ThreadOfGUID answers what Email.threadId and Thread/get report.
func (s *State) ThreadOfGUID(guid string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.threadOfGUID(guid)
}

func (s *State) threadOfGUID(guid string) (string, bool) {
	id, ok := s.byGUID[guid]
	if !ok {
		return s.fromBase(secGUID, guid)
	}
	return s.resolve(id), true
}

// GUIDsOfThread lists the messages in a conversation, following aliases so a
// merged thread answers with everything that joined it.
func (s *State) GUIDsOfThread(threadID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.guidsOfThread(threadID)
}

func (s *State) guidsOfThread(threadID string) []string {
	if s.pair != nil {
		out, err := s.pair.Members(threadID)
		if err != nil {
			slog.Debug("threads: base unreadable, members answered from the log", "path", s.pair.path, "err", err)
		}
		sort.Strings(out)
		return out
	}
	want := s.resolve(threadID)
	var out []string
	for guid, id := range s.byGUID {
		if s.resolve(id) == want {
			out = append(out, guid)
		}
	}
	sort.Strings(out)
	return out
}

// Threads lists every conversation that still exists, merged ones excluded.
func (s *State) Threads() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.threads()
}

func (s *State) threads() []string {
	seen := map[string]bool{}
	for _, id := range s.byGUID {
		seen[s.resolve(id)] = true
	}
	if s.pair != nil {
		_ = s.pair.baseThreads(func(id string) {
			if _, gone := s.aliasOf[id]; !gone {
				seen[id] = true
			}
		})
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Placement is one message's assignment, ready to be appended.
type Placement struct {
	GUID       string
	MessageID  string
	SubjectKey string
	ThreadID   string
	// MergedFrom are threads this message folded into ThreadID.
	MergedFrom []string
	// ContinuesFrom is the full conversation ThreadID, new, continues.
	ContinuesFrom string
}

// Append writes a placement to the log and applies it to s.
//
// The caller holds the account's thread lock: this is a read-modify-write in
// the sense that the placement was computed from s, and two deliveries
// computing from the same state would otherwise assign two thread ids to one
// conversation.
func Append(path string, s *State, p Placement) error {
	if p.ThreadID == "" {
		return fmt.Errorf("threads: placement for %s has no thread", p.GUID)
	}
	// No fsync here, and that is a decision rather than an omission:
	//
	//  1. it does not remove the window, only narrows it -- an OS crash tears
	//     between synced writes as readily as within one;
	//  2. the real protection is the record ORDER below: whatever survives a
	//     truncated tail is the harmless half;
	//  3. this sidecar is DERIVED state. The migration step that builds it for
	//     existing accounts is also the tool that rebuilds it, so a lost tail
	//     is recoverable -- unlike mail, which is not.
	//
	// Paying an fsync per delivery for a rebuildable derived structure is the
	// wrong trade. If a field case ever shows tails going missing
	// systematically -- something other than a crash -- this comes back with a
	// number behind it.
	//
	// Aliases first, and the asymmetry is the reason. A crash mid-write can
	// truncate the tail; whichever records are lost, the survivors must be the
	// safe half. An alias with no G record is a thread pointing at a thread --
	// harmless, it names a conversation that simply has one fewer message. A G
	// record with no alias is a merge that did not happen: the two halves of
	// one conversation stay apart, permanently. So the dangerous record goes
	// last.
	var b strings.Builder
	for _, old := range p.MergedFrom {
		writeRec(&b, recAlias, old, p.ThreadID)
	}
	if p.MessageID != "" {
		writeRec(&b, recMessage, p.MessageID, p.ThreadID)
	}
	if p.SubjectKey != "" {
		writeRec(&b, recSubject, p.SubjectKey, p.ThreadID)
	}
	if p.ContinuesFrom != "" {
		writeRec(&b, recNext, p.ContinuesFrom, p.ThreadID)
	}
	writeRec(&b, recGUID, p.GUID, p.ThreadID)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("threads: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("threads: open %s: %w", path, err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close() //nolint:errcheck
		return fmt.Errorf("threads: append %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("threads: close %s: %w", path, err)
	}

	// Apply after the write succeeded, so an in-memory state never claims a
	// placement the file does not carry -- and all at once, so a reader sees
	// the placement with its alias or neither.
	s.applyPlacement(p)
	return nil
}

// applyPlacement takes a placement the file already carries into the maps.
func (s *State) applyPlacement(p Placement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// In the order Append writes them, so the counts move as a fold of the
	// file moves them.
	for _, old := range p.MergedFrom {
		s.applyLocked(recAlias, old, p.ThreadID)
	}
	if p.MessageID != "" {
		s.applyLocked(recMessage, p.MessageID, p.ThreadID)
	}
	if p.SubjectKey != "" {
		s.applyLocked(recSubject, p.SubjectKey, p.ThreadID)
	}
	if p.ContinuesFrom != "" {
		s.applyLocked(recNext, p.ContinuesFrom, p.ThreadID)
	}
	s.applyLocked(recGUID, p.GUID, p.ThreadID)
}

// writeRec appends one record. Values are flattened rather than escaped: a
// TAB inside a Message-ID or a subject key would shift the fields, and this
// file is read by position. Message-IDs do not legally contain one, and a
// subject key that does is a malformed header, not a record shape.
func writeRec(b *strings.Builder, typ, key, val string) {
	b.WriteString(typ)
	b.WriteByte('\t')
	b.WriteString(flatten(key))
	b.WriteByte('\t')
	b.WriteString(flatten(val))
	b.WriteByte('\n')
}

func flatten(s string) string {
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
}

// Compact rewrites the log as the smallest set of records that folds to the
// same state: aliases resolved away, one record per key.
//
// Aliases are the reason this is not optional. A merge is O(1) on the delivery
// path precisely because it appends an alias instead of rewriting membership,
// and a long-lived account accumulates them; folding them here is what keeps
// resolve() short and the file from growing without bound.
//
// tmp+rename, so a reader either sees the old file or the new one. The caller
// holds the account's thread lock, as for Append.
func Compact(path string, s *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var recs []string
	for guid, id := range s.byGUID {
		recs = append(recs, record(recGUID, guid, s.resolve(id)))
	}
	for msg, id := range s.byMessage {
		recs = append(recs, record(recMessage, msg, s.resolve(id)))
	}
	for key, id := range s.bySubject {
		recs = append(recs, record(recSubject, key, s.resolve(id)))
	}
	for full, id := range s.next {
		recs = append(recs, record(recNext, s.resolve(full), s.resolve(id)))
	}
	// Sorted, so two processes compacting the same state produce byte-identical
	// files -- the same rule the subscriptions file follows.
	sort.Strings(recs)
	// The generation header goes first and is not sorted with the rest: a
	// reader has to know which history it is reading before it reads any of it.
	gen := s.generation + 1
	recs = append([]string{record(recGen, strconv.FormatUint(gen, 10), strconv.FormatInt(time.Now().Unix(), 10))}, recs...)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("threads: mkdir: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("threads: open %s: %w", tmp, err)
	}
	w := bufio.NewWriter(f)
	for _, r := range recs {
		if _, err := w.WriteString(r); err != nil {
			f.Close()      //nolint:errcheck
			os.Remove(tmp) //nolint:errcheck
			return fmt.Errorf("threads: write %s: %w", tmp, err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()      //nolint:errcheck
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("threads: flush %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("threads: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("threads: rename %s: %w", tmp, err)
	}
	// The log now holds the whole history: a base under it would pair with a
	// log that continues none of it.
	if err := os.Remove(path + BaseSuffix); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("threads: drop the base under %s: %w", path, err)
	}
	// The file now carries resolved values, so the live state must too --
	// BOTH halves. Dropping the aliases while leaving the maps pointing at
	// swallowed threads is worse than doing nothing: the next delivery
	// computed from this state would place a reply into a thread that no
	// longer exists, splitting a conversation that had already been merged.
	// That is the permanent wrong answer this whole design exists to avoid,
	// and it would be introduced by the tidying step.
	for k, id := range s.byGUID {
		s.byGUID[k] = s.resolve(id)
	}
	for k, id := range s.byMessage {
		s.byMessage[k] = s.resolve(id)
	}
	for k, id := range s.bySubject {
		s.bySubject[k] = s.resolve(id)
	}
	next := make(map[string]string, len(s.next))
	for full, id := range s.next {
		next[s.resolve(full)] = s.resolve(id)
	}
	s.next = next
	for old := range s.aliasOf {
		delete(s.aliasOf, old)
	}
	s.generation = gen
	s.records = len(recs)
	return nil
}

func record(typ, key, val string) string {
	var b strings.Builder
	writeRec(&b, typ, key, val)
	return b.String()
}

// Read runs fn while holding the state still.
//
// One request, one state. A reader that took the lock per lookup could answer
// from two states -- the thread of a message read before a merge, its member
// list read after -- and the client would receive a conversation that never
// existed. The hold is nanoseconds: the write side is a few map assignments,
// already off the file I/O.
func (s *State) Read(fn func(View)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(View{s: s})
}

// View is the account's conversations, held still for the duration of one
// Read. Its methods do not lock: the lock is the Read around them.
type View struct{ s *State }

// ThreadOf reports the conversation a message belongs to.
func (v View) ThreadOf(guid string) (string, bool) { return v.s.threadOfGUID(guid) }

// Members lists the messages of a conversation, following merges so an id a
// client cached still answers with the conversation it became.
func (v View) Members(threadID string) []string { return v.s.guidsOfThread(threadID) }

// Threads lists every conversation the account has.
func (v View) Threads() []string { return v.s.threads() }

// Changes is what happened to an account's conversations between two states.
type Changes struct {
	// Created are conversations that did not exist at the old state.
	Created []string
	// Updated are conversations that gained a message, or absorbed another.
	Updated []string
	// Destroyed are conversations that were merged into another and no longer
	// exist under their own id. RFC 8621 expects a client to learn this: a
	// merge RENAMES the thread of every message that was in the swallowed one,
	// and a client that never hears about it holds an id nothing answers for.
	Destroyed []string
	// Head is the state to report back, and the one to ask from next time.
	Head int
	// Generation is the compaction these positions belong to. A caller holding
	// a position from another one must refuse rather than diff: the records it
	// would read describe a file that no longer exists.
	Generation uint64
}

// Generation is how many times this account's log has been compacted. A
// position from another generation describes a file that no longer exists.
func (s *State) Generation() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation
}

// Head is the account's current threading state: how many records its log
// carries.
//
// The log IS the change journal here, unlike the Email state, which is derived
// from folder markers because no journal of message changes exists. That is
// why this state is a position rather than a description: the records between
// two positions are exactly what happened.
func (s *State) Head() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.records
}

// ChangesSince replays the log from a position and reports what changed.
//
// Read from the file rather than from a folded state, because a fold has
// forgotten the order things happened in -- and that order is the answer. It
// streams: a client asking "what changed since a moment ago" reads the tail,
// not the account.
func ChangesSince(path string, since int) (Changes, error) {
	if _, err := os.Stat(path + BaseSuffix); err == nil {
		return changesSincePair(path, since)
	}
	return changesSinceLog(path, since)
}

// changesSinceLog is ChangesSince over a log that is the whole history.
func changesSinceLog(path string, since int) (Changes, error) {
	ch := Changes{Head: since}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No sidecar: nothing has ever happened, which is the honest answer
			// for an account the migration step has not reached.
			ch.Head = 0
			return ch, nil
		}
		return ch, fmt.Errorf("threads: open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	// Threads seen before the requested position: a conversation that already
	// existed is updated by a later record, not created by it.
	known := map[string]bool{}
	created := map[string]bool{}
	updated := map[string]bool{}
	destroyed := map[string]bool{}

	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) != 3 {
			continue
		}
		n++
		typ, key, val := fields[0], fields[1], fields[2]
		if typ == recGen {
			if g, perr := strconv.ParseUint(key, 10, 64); perr == nil {
				ch.Generation = g
			}
			continue
		}
		if n <= since {
			if typ == recGUID {
				known[val] = true
			}
			if typ == recAlias {
				// A thread that was already merged away before the client's
				// state is not news to it.
				known[key] = false
				delete(known, key)
			}
			continue
		}
		switch typ {
		case recGUID:
			if known[val] || created[val] {
				updated[val] = true
			} else {
				created[val] = true
			}
			known[val] = true
		case recAlias:
			// key folded into val: the client loses one conversation and the
			// other grew.
			if created[key] {
				// Created and swallowed within the window: the client never
				// saw it, so it has nothing to forget.
				delete(created, key)
			} else {
				destroyed[key] = true
			}
			delete(updated, key)
			if !created[val] {
				updated[val] = true
			}
			known[val] = true
		}
	}
	if err := sc.Err(); err != nil {
		return ch, fmt.Errorf("threads: read %s: %w", path, err)
	}
	ch.Head = n
	ch.Created = sortedKeys(created)
	ch.Updated = sortedKeys(updated)
	ch.Destroyed = sortedKeys(destroyed)
	return ch, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, ok := range m {
		if ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
