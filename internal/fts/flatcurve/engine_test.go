//go:build flatcurve

package flatcurve

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0kaba0hub/go-xapian"

	"github.com/yarilomail/yarilo/pkg/fts"
)

func testEngine(t *testing.T, opts Options) (fts.UserIndex, fts.UserRef) {
	t.Helper()
	user := fts.UserRef{Username: "u@test", IndexRoot: t.TempDir()}
	ui, err := New(opts).OpenUser(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck
	return ui, user
}

var inbox = fts.MailboxRef{GUID: "g1", Name: "INBOX", UIDValidity: 1}

// indexDoc feeds one message's tokens (subject header + body words) into
// inbox. indexDocIn is the general form for tests that need a second
// mailbox (#715's OptimizeMailbox isolation test).
// testGUID is the identity a uid stands for in these rows: the engine answers
// with messages, and a row reads them back as the uids it indexed.
func testGUID(uid uint32) [16]byte {
	var g [16]byte
	binary.BigEndian.PutUint32(g[:4], uid)
	return g
}

// uidsOf reads an engine answer back as uids through the same rule.
func uidsOf(guids [][16]byte) []uint32 {
	if len(guids) == 0 {
		return nil
	}
	out := make([]uint32, 0, len(guids))
	for _, g := range guids {
		out = append(out, binary.BigEndian.Uint32(g[:4]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func indexDoc(t *testing.T, ui fts.UserIndex, uid uint32, subject []string, body []string) {
	t.Helper()
	indexDocIn(t, ui, inbox, uid, subject, body)
}

func indexDocIn(t *testing.T, ui fts.UserIndex, mbox fts.MailboxRef, uid uint32, subject []string, body []string) {
	t.Helper()
	indexCopy(t, ui, mbox, uid, testGUID(uid), subject, body)
}

// indexCopy indexes one copy of a message: the same GUID under another folder
// and uid is a copy, not another message.
func indexCopy(t *testing.T, ui fts.UserIndex, mbox fts.MailboxRef, uid uint32, guid [16]byte, subject []string, body []string) {
	t.Helper()
	up, err := ui.BeginUpdate(mbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(subject) > 0 {
		ok, err := up.SetBuildKey(fts.BuildKey{UID: uid, GUID: guid, Type: fts.KeyHeader, HdrName: "subject"})
		if err != nil || !ok {
			t.Fatalf("subject key: ok=%v err=%v", ok, err)
		}
		for _, tok := range subject {
			if err := up.BuildMore([]byte(tok)); err != nil {
				t.Fatal(err)
			}
		}
	}
	ok, err := up.SetBuildKey(fts.BuildKey{UID: uid, GUID: guid, Type: fts.KeyBodyPart, ContentType: "text/plain"})
	if err != nil || !ok {
		t.Fatalf("body key: ok=%v err=%v", ok, err)
	}
	for _, tok := range body {
		if err := up.BuildMore([]byte(tok)); err != nil {
			t.Fatal(err)
		}
	}
	if err := up.Commit(); err != nil {
		t.Fatal(err)
	}
}

func bodyQuery(words ...string) fts.Query {
	var ws []fts.Word
	for _, w := range words {
		ws = append(ws, fts.Word{Variants: []string{w}})
	}
	return fts.Query{Terms: []fts.Term{{Field: fts.FieldBody, Words: ws}}, AndTerms: true}
}

func TestIndexAndLookup(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 1, []string{"quarterly", "report"}, []string{"budget", "review"})
	indexDoc(t, ui, 2, []string{"lunch"}, []string{"budget", "pizza"})

	tests := []struct {
		name     string
		q        fts.Query
		definite []uint32
		maybe    []uint32
	}{
		{"body single word", bodyQuery("budget"), []uint32{1, 2}, nil},
		{"body AND words", bodyQuery("budget", "pizza"), []uint32{2}, nil},
		{"body prefix wildcard", bodyQuery("bud"), []uint32{1, 2}, nil},
		{"body no match", bodyQuery("zzz"), nil, nil},
		{
			"indexed header field",
			fts.Query{Terms: []fts.Term{{Field: fts.FieldHeader, HdrName: "subject",
				Words: []fts.Word{{Variants: []string{"lunch"}}}}}, AndTerms: true},
			[]uint32{2}, nil,
		},
		{
			"text matches header and body",
			fts.Query{Terms: []fts.Term{{Field: fts.FieldText,
				Words: []fts.Word{{Variants: []string{"quarterly"}}}}}, AndTerms: true},
			[]uint32{1}, nil,
		},
		{
			"non-indexed header is maybe",
			fts.Query{Terms: []fts.Term{{Field: fts.FieldHeader, HdrName: "x-custom",
				Words: []fts.Word{{Variants: []string{"quarterly"}}}}}, AndTerms: true},
			nil, []uint32{1},
		},
		{
			"variants OR within word",
			fts.Query{Terms: []fts.Term{{Field: fts.FieldBody,
				Words: []fts.Word{{Variants: []string{"zzz", "pizza"}}}}}, AndTerms: true},
			[]uint32{2}, nil,
		},
		{
			"NOT excludes",
			fts.Query{Terms: []fts.Term{
				{Field: fts.FieldBody, Words: []fts.Word{{Variants: []string{"budget"}}}},
				{Field: fts.FieldBody, Words: []fts.Word{{Variants: []string{"pizza"}}}, Not: true},
			}, AndTerms: true},
			[]uint32{1}, nil,
		},
		{
			"header existence probe",
			fts.Query{Terms: []fts.Term{{Field: fts.FieldHeader, HdrName: "subject"}}, AndTerms: true},
			[]uint32{1, 2}, nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ui.Lookup([]string{inbox.GUID}, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			definite, maybe := uidsOf(res.DefiniteGUIDs), uidsOf(res.MaybeGUIDs)
			if !reflect.DeepEqual(definite, tc.definite) || !reflect.DeepEqual(maybe, tc.maybe) {
				t.Fatalf("definite=%v maybe=%v, want %v / %v",
					definite, maybe, tc.definite, tc.maybe)
			}
		})
	}
}

func TestUppercaseFirstCharHack(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	// The indexer lowercases a leading ASCII capital so it is not mistaken
	// for a Xapian prefix; the query side must apply the same rule.
	indexDoc(t, ui, 1, nil, []string{"Zebra"})
	res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("Zebra"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(uidsOf(res.DefiniteGUIDs), []uint32{1}) {
		t.Fatalf("capitalized term lookup = %v", uidsOf(res.DefiniteGUIDs))
	}
}

func TestExpunge(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 1, nil, []string{"alpha"})
	indexDoc(t, ui, 2, nil, []string{"alpha"})
	if err := ui.Expunge(inbox, 1); err != nil {
		t.Fatal(err)
	}
	res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(uidsOf(res.DefiniteGUIDs), []uint32{2}) {
		t.Fatalf("after expunge = %v, want [2]", uidsOf(res.DefiniteGUIDs))
	}
	// Expunging a missing UID is a no-op.
	if err := ui.Expunge(inbox, 99); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpoint(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	last, uidv, sum, err := ui.Checkpoint(inbox)
	if err != nil || last != 0 || uidv != 0 || sum != 0 {
		t.Fatalf("empty checkpoint = %d/%d/%d/%v", last, uidv, sum, err)
	}
	if err := ui.SetCheckpoint(inbox, 42, 99, 7); err != nil {
		t.Fatal(err)
	}
	last, uidv, sum, err = ui.Checkpoint(inbox)
	if err != nil || last != 42 || uidv != 99 || sum != 7 {
		t.Fatalf("checkpoint = %d/%d/%d/%v, want 42/99/7", last, uidv, sum, err)
	}
}

// TestCheckpointLegacyV1 verifies a v1 checkpoint file ("1 <uid> <sum>") still
// reads back, with uidvalidity 0 so a UIDVALIDITY mismatch resets it (#638).
func TestCheckpointLegacyV1(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	dir := (ui.(*userIndex)).state().dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointPath(dir, inbox), []byte("1 10 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	last, uidv, sum, err := ui.Checkpoint(inbox)
	if err != nil || last != 10 || uidv != 0 || sum != 7 {
		t.Fatalf("legacy v1 checkpoint = %d/%d/%d/%v, want 10/0/7", last, uidv, sum, err)
	}
}

// A missing checkpoint is "never indexed": the highest docid says nothing
// about a uid now, so there is nothing to read it off (#1986).
func TestAMissingCheckpointReadsAsNeverIndexed(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 17, nil, []string{"legacy"})
	dir := ui.(*userIndex).state().dir
	if err := os.Remove(checkpointPath(dir, inbox)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	last, uidv, sum, err := ui.Checkpoint(inbox)
	if err != nil || last != 0 || uidv != 0 || sum != 0 {
		t.Fatalf("checkpoint = %d/%d/%d/%v, want 0/0/0", last, uidv, sum, err)
	}
}

func TestRescanTargeted(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	for uid := uint32(1); uid <= 5; uid++ {
		indexDoc(t, ui, uid, nil, []string{"word"})
	}
	// Mailbox now holds 2,3,5,7: 1 and 4 were expunged offline; 7 is new.
	missing, err := ui.Rescan(inbox, copiesOf(2, 3, 5, 7))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, []uint32{7}) {
		t.Fatalf("missing = %v, want [7]", missing)
	}
	res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("word"))
	if err != nil {
		t.Fatal(err)
	}
	// Stale 1 and 4 removed; 2,3,5 intact (no delete-above-gap storm).
	if !reflect.DeepEqual(uidsOf(res.DefiniteGUIDs), []uint32{2, 3, 5}) {
		t.Fatalf("after rescan = %v, want [2 3 5]", uidsOf(res.DefiniteGUIDs))
	}
}

// TestRotateTimeTriggersRotationOnSlowCommit (#724) proves commitCurrent
// rotates when a commit exceeds RotateTime, independent of RotateCount. A
// tiny RotateTime (1ns) makes ANY real commit exceed it deterministically —
// no fake clock or artificial sleep needed, no flakiness from actual timing.
// RotateCount is set far out of reach so only the time-based trigger could
// possibly cause the rotation seen here.
func TestRotateTimeTriggersRotationOnSlowCommit(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1000, CommitLimit: 1, RotateTime: time.Nanosecond})
	indexDoc(t, ui, 1, nil, []string{"alpha"})
	dir := ui.(*userIndex).state().dir
	sealed, current := countShards(t, dir)
	if sealed < 1 {
		t.Fatalf("expected a time-based rotation after the commit: sealed=%d current=%d", sealed, current)
	}
}

// TestRotateTimeZeroDisablesTimeBasedRotation (#724) proves RotateTime: 0
// truly disables the time-based trigger, rather than withDefaults()
// silently coercing it back to the positive default (5000ms) the way
// OptimizeLimit used to before #715.
func TestRotateTimeZeroDisablesTimeBasedRotation(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1000, CommitLimit: 1, RotateTime: 0})
	indexDoc(t, ui, 1, nil, []string{"alpha"})
	dir := ui.(*userIndex).state().dir
	sealed, current := countShards(t, dir)
	if sealed != 0 || current != 1 {
		t.Fatalf("expected no rotation with RotateTime=0: sealed=%d current=%d", sealed, current)
	}
}

func TestRotationAndOptimize(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 2})
	for uid := uint32(1); uid <= 5; uid++ {
		indexDoc(t, ui, uid, nil, []string{"steady"})
	}
	dir := ui.(*userIndex).state().dir
	sealed, current := countShards(t, dir)
	if sealed < 2 {
		t.Fatalf("expected rotation to seal shards: sealed=%d current=%d", sealed, current)
	}
	res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("steady"))
	if err != nil {
		t.Fatal(err)
	}
	if len(uidsOf(res.DefiniteGUIDs)) != 5 {
		t.Fatalf("lookup across shards = %v", uidsOf(res.DefiniteGUIDs))
	}
	// Whole-user optimize is a loop over Mailboxes() under each mailbox's
	// own lock now (#1176); the service does exactly this.
	for _, mbox := range ui.Mailboxes() {
		if err := ui.OptimizeMailbox(mbox); err != nil {
			t.Fatal(err)
		}
	}
	sealed, current = countShards(t, dir)
	if sealed != 1 || current != 0 {
		t.Fatalf("after optimize: sealed=%d current=%d, want 1/0", sealed, current)
	}
	res, err = ui.Lookup([]string{inbox.GUID}, bodyQuery("steady"))
	if err != nil || len(uidsOf(res.DefiniteGUIDs)) != 5 {
		t.Fatalf("lookup after optimize = %v (%v)", uidsOf(res.DefiniteGUIDs), err)
	}
}

func countShards(t *testing.T, dir string) (sealed, current int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		switch {
		case strings.HasPrefix(e.Name(), dbPrefix):
			sealed++
		case strings.HasPrefix(e.Name(), currentPrefix):
			current++
		}
	}
	return sealed, current
}

// TestOptimizeCallbackFiresAtLimit (#715) proves rotate() drives the
// OptimizeNotifier callback: it stays silent below OptimizeLimit and fires
// (with the correct mailbox) as soon as the sealed-shard count reaches it.
func TestOptimizeCallbackFiresAtLimit(t *testing.T) {
	eng := New(Options{RotateCount: 2, OptimizeLimit: 3})
	var mu sync.Mutex
	var calls []fts.MailboxRef
	eng.SetOptimizeCallback(func(_ fts.UserRef, m fts.MailboxRef) {
		mu.Lock()
		calls = append(calls, m)
		mu.Unlock()
	})
	user := fts.UserRef{Username: "u@test", IndexRoot: t.TempDir()}
	ui, err := eng.OpenUser(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck

	// RotateCount=2: 4 docs seal exactly 2 shards — below OptimizeLimit=3.
	for uid := uint32(1); uid <= 4; uid++ {
		indexDoc(t, ui, uid, nil, []string{"x"})
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("callback fired %d times before reaching OptimizeLimit (2 sealed shards)", n)
	}

	// 2 more docs seal a 3rd shard — now at OptimizeLimit.
	for uid := uint32(5); uid <= 6; uid++ {
		indexDoc(t, ui, uid, nil, []string{"x"})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("callback never fired after reaching OptimizeLimit")
	}
	// The index is the user's, so the callback names no folder: what it asks
	// for is a compaction of that one index (#1986).
	if calls[0].GUID != "" {
		t.Fatalf("callback names folder %+v, but the index is the user's", calls[0])
	}
}

// TestOptimizeCallbackDisabledWhenLimitZero (#715) proves OptimizeLimit: 0
// truly disables auto-optimize, rather than withDefaults() silently
// coercing it back to the positive default (10) the way it used to.
func TestOptimizeCallbackDisabledWhenLimitZero(t *testing.T) {
	eng := New(Options{RotateCount: 2, OptimizeLimit: 0})
	var calls atomic.Int32
	eng.SetOptimizeCallback(func(fts.UserRef, fts.MailboxRef) { calls.Add(1) })
	user := fts.UserRef{Username: "u@test", IndexRoot: t.TempDir()}
	ui, err := eng.OpenUser(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck

	// 30 docs / RotateCount=2 seals 15 shards — well past even the old
	// (buggy) implicit default of 10, so this decisively catches a
	// regression back to "0 silently becomes 10" rather than passing by
	// accident from too few shards.
	for uid := uint32(1); uid <= 30; uid++ {
		indexDoc(t, ui, uid, nil, []string{"x"})
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("callback fired %d times with OptimizeLimit=0 (must be disabled)", n)
	}
}

// TestOptimizeMailboxIsolatesOtherMailboxes (#715) proves OptimizeMailbox
// compacts exactly the requested mailbox, leaving a different mailbox's
// shards untouched — unlike whole-user Optimize.
// One index per user: a compaction is the user's, and both folders' documents
// are in the shards it merges (#1986).
func TestOptimizeMergesTheUsersShards(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 2})
	archive := fts.MailboxRef{GUID: "g2", Name: "Archive", UIDValidity: 1}
	for uid := uint32(1); uid <= 2; uid++ {
		indexDocIn(t, ui, inbox, uid, nil, []string{"shared"})
	}
	for uid := uint32(3); uid <= 4; uid++ {
		indexDocIn(t, ui, archive, uid, nil, []string{"shared"})
	}
	dir := ui.(*userIndex).state().dir
	before, err := shardPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) < 2 {
		t.Fatalf("the fixture left %d shards, the row needs at least two", len(before))
	}

	if err := ui.OptimizeMailbox(fts.MailboxRef{}); err != nil {
		t.Fatal(err)
	}
	after, err := shardPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Errorf("after a compaction the index holds %d shards, want one", len(after))
	}
	for _, mbox := range []fts.MailboxRef{inbox, archive} {
		res, lerr := ui.Lookup([]string{mbox.GUID}, fts.Query{AndTerms: true,
			Terms: []fts.Term{{Field: fts.FieldBody, Words: []fts.Word{{Variants: []string{"shared"}}}}}})
		if lerr != nil {
			t.Fatal(lerr)
		}
		if len(uidsOf(res.DefiniteGUIDs)) != 2 {
			t.Errorf("folder %q answers %v after the compaction, want two messages",
				mbox.Name, uidsOf(res.DefiniteGUIDs))
		}
	}
}

// TestShardPathsIgnoresOptimizeTmpDir (#715) is the direct safety check
// behind the lazy-cleanup design: a leftover "optimize" compaction tmp dir
// must never be mistaken for a shard by shardPaths — otherwise a stale tmp
// dir left by a crash would corrupt every subsequent Lookup/Optimize, not
// just waste disk space until the next lazy cleanup.
func TestShardPathsIgnoresOptimizeTmpDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "optimize"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, dbPrefix+"1"), 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := shardPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if filepath.Base(p) == "optimize" {
			t.Fatal("shardPaths must not pick up the optimize tmp dir as a shard")
		}
	}
	if len(paths) != 1 {
		t.Fatalf("shardPaths = %v, want exactly the one dbPrefix dir", paths)
	}
}

// TestCleanStaleOptimizeTmpDir (#715) proves a leftover "optimize" tmp dir
// from a prior crash is swept the first time the mailbox's directory is
// touched — the "lazy, on first state() open" substitute for a startup
// sweep the service has no way to do upfront (no list of every mailbox).
func TestCleanStaleOptimizeTmpDir(t *testing.T) {
	user := fts.UserRef{Username: "u@test", IndexRoot: t.TempDir()}
	ui, err := New(Options{}).OpenUser(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ui.Close() }) //nolint:errcheck

	dir := ui.(*userIndex).eng.opts.Store.Locate(user)
	tmp := filepath.Join(dir, "optimize")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "junk"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	indexDoc(t, ui, 1, nil, []string{"hi"}) // first touch of this mailbox's state

	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("stale optimize tmp dir not cleaned: stat err=%v", err)
	}
}

func TestSubstringSearch(t *testing.T) {
	ui, _ := testEngine(t, Options{SubstringSearch: true})
	indexDoc(t, ui, 1, nil, []string{"butterfly"})
	// Substring mode stores suffixes, so an inner fragment prefix-matches.
	res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("tterf"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(uidsOf(res.DefiniteGUIDs), []uint32{1}) {
		t.Fatalf("substring lookup = %v, want [1]", uidsOf(res.DefiniteGUIDs))
	}
	// Without substring mode the same fragment must not match.
	ui2, _ := testEngine(t, Options{})
	indexDoc(t, ui2, 1, nil, []string{"butterfly"})
	res, err = ui2.Lookup([]string{inbox.GUID}, bodyQuery("tterf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(uidsOf(res.DefiniteGUIDs)) != 0 {
		t.Fatalf("prefix-only lookup matched inner fragment: %v", uidsOf(res.DefiniteGUIDs))
	}
}

func TestMinTermSize(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 1, nil, []string{"a", "ok", "xyz"})
	// 1-byte token is below min_term_size (2) and never indexed.
	res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("ok"))
	if err != nil || len(uidsOf(res.DefiniteGUIDs)) != 1 {
		t.Fatalf("2-byte term should be indexed: %v (%v)", uidsOf(res.DefiniteGUIDs), err)
	}
	res, err = ui.Lookup([]string{inbox.GUID}, bodyQuery("a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(uidsOf(res.DefiniteGUIDs)) != 0 {
		t.Fatalf("1-byte term must not be indexed: %v", uidsOf(res.DefiniteGUIDs))
	}
}

func TestVersionMetadataWritten(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	indexDoc(t, ui, 1, nil, []string{"word"})
	if err := ui.Close(); err != nil {
		t.Fatal(err)
	}
	dir := ui.(*userIndex).state().dir
	paths, err := shardPaths(dir)
	if err != nil || len(paths) == 0 {
		t.Fatalf("no shards: %v", err)
	}
	w, err := xapian.OpenWDB(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	got, err := w.GetMetadata(versionKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != versionValue {
		t.Fatalf("version metadata = %q, want %q", got, versionValue)
	}
}

// One index per user, wherever the mail lives: the folder is a term in a
// document, not a directory, and the driver does not appear in the path (#1986).
func TestTheIndexIsTheUsersWhateverTheDriver(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, Label)
	for _, driver := range []string{"mdbox", "sdbox", "maildir", ""} {
		user := fts.UserRef{Username: "u@test", IndexRoot: root, Driver: driver}
		ui, err := New(Options{}).OpenUser(context.Background(), user)
		if err != nil {
			t.Fatalf("driver %q: OpenUser: %v", driver, err)
		}
		if got := ui.(*userIndex).state().dir; got != want {
			t.Errorf("driver %q: dir = %q, want %q", driver, got, want)
		}
		ui.Close() //nolint:errcheck
	}
}

// TestMultiShardLookupReturnsRealUIDs is the #670 regression: once a mailbox's
// index rotates into more than one shard, a combined-database search would
// report Xapian's interleaved external docids instead of the real UIDs. Force
// rotation (RotateCount 3) so 10 messages span several shards, then assert
// SEARCH returns the actual injected UIDs.
func TestMultiShardLookupReturnsRealUIDs(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 3})
	var want []uint32
	for uid := uint32(1); uid <= 10; uid++ {
		if uid%2 == 0 {
			indexDoc(t, ui, uid, nil, []string{"needle"})
			want = append(want, uid)
		} else {
			indexDoc(t, ui, uid, nil, []string{"filler"})
		}
	}
	dir := ui.(*userIndex).state().dir
	if sealed, _ := countShards(t, dir); sealed < 2 {
		t.Fatalf("expected rotation to seal ≥2 shards, got sealed=%d", sealed)
	}
	res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("needle"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(uidsOf(res.DefiniteGUIDs), want) {
		t.Fatalf("multi-shard lookup = %v, want %v (real UIDs, not interleaved docids)", uidsOf(res.DefiniteGUIDs), want)
	}
}

// TestHeaderExistenceRequiresRealToken (#725 item 6) proves the
// header-existence boolean term is set only once a real (>=MinTermSize)
// token is confirmed for that field, not proactively on SetBuildKey: a
// value that tokenizes to nothing must not satisfy a HEADER existence
// probe.
func TestHeaderExistenceRequiresRealToken(t *testing.T) {
	ui, _ := testEngine(t, Options{MinTermSize: 2})
	up, err := ui.BeginUpdate(inbox)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := up.SetBuildKey(fts.BuildKey{UID: 1, GUID: testGUID(1), Type: fts.KeyHeader, HdrName: "x-empty"})
	if err != nil || !ok {
		t.Fatalf("SetBuildKey: ok=%v err=%v", ok, err)
	}
	if err := up.BuildMore([]byte("a")); err != nil { // below MinTermSize=2
		t.Fatal(err)
	}
	ok, err = up.SetBuildKey(fts.BuildKey{UID: 1, GUID: testGUID(1), Type: fts.KeyHeader, HdrName: "x-real"})
	if err != nil || !ok {
		t.Fatalf("SetBuildKey: ok=%v err=%v", ok, err)
	}
	if err := up.BuildMore([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := up.Commit(); err != nil {
		t.Fatal(err)
	}

	probe := func(hdr string) []uint32 {
		t.Helper()
		res, err := ui.Lookup([]string{inbox.GUID}, fts.Query{
			Terms:    []fts.Term{{Field: fts.FieldHeader, HdrName: hdr}},
			AndTerms: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return uidsOf(res.DefiniteGUIDs)
	}
	if got := probe("x-empty"); len(got) != 0 {
		t.Fatalf("HEADER x-empty existence probe = %v, want none (zero real tokens)", got)
	}
	if got := probe("x-real"); !reflect.DeepEqual(got, []uint32{1}) {
		t.Fatalf("HEADER x-real existence probe = %v, want [1]", got)
	}
}

// TestHeaderNameIndexedSeparately (#725 item 5) proves the header NAME
// itself is searchable via TEXT (the A-pool), and that it does NOT also
// satisfy a HEADER <name> VALUE search for that same literal name — the
// name and the value are indexed under separate build keys.
func TestHeaderNameIndexedSeparately(t *testing.T) {
	ui, _ := testEngine(t, Options{})
	up, err := ui.BeginUpdate(inbox)
	if err != nil {
		t.Fatal(err)
	}
	// The header name build key: empty HdrName, per buildmail's contract.
	ok, err := up.SetBuildKey(fts.BuildKey{UID: 1, GUID: testGUID(1), Type: fts.KeyHeader})
	if err != nil || !ok {
		t.Fatalf("SetBuildKey (name): ok=%v err=%v", ok, err)
	}
	if err := up.BuildMore([]byte("list")); err != nil {
		t.Fatal(err)
	}
	if err := up.BuildMore([]byte("id")); err != nil {
		t.Fatal(err)
	}
	// The value build key: a value that shares no words with the name.
	ok, err = up.SetBuildKey(fts.BuildKey{UID: 1, GUID: testGUID(1), Type: fts.KeyHeader, HdrName: "list-id"})
	if err != nil || !ok {
		t.Fatalf("SetBuildKey (value): ok=%v err=%v", ok, err)
	}
	if err := up.BuildMore([]byte("project")); err != nil {
		t.Fatal(err)
	}
	if err := up.Commit(); err != nil {
		t.Fatal(err)
	}

	// TEXT "list" matches — the header NAME reached the A-pool.
	res, err := ui.Lookup([]string{inbox.GUID}, fts.Query{
		Terms:    []fts.Term{{Field: fts.FieldText, Words: []fts.Word{{Variants: []string{"list"}}}}},
		AndTerms: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(uidsOf(res.DefiniteGUIDs), []uint32{1}) {
		t.Fatalf("TEXT %q = %v, want [1] (header name must reach the A-pool)", "list", uidsOf(res.DefiniteGUIDs))
	}

	// HEADER list-id "list" must NOT match — the name's tokens must not
	// leak into the per-field H<NAME> pool alongside the value.
	res, err = ui.Lookup([]string{inbox.GUID}, fts.Query{
		Terms:    []fts.Term{{Field: fts.FieldHeader, HdrName: "list-id", Words: []fts.Word{{Variants: []string{"list"}}}}},
		AndTerms: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(uidsOf(res.DefiniteGUIDs)) != 0 {
		t.Fatalf("HEADER list-id %q = %v, want none (name tokens must not leak into the value pool)", "list", uidsOf(res.DefiniteGUIDs))
	}
}
