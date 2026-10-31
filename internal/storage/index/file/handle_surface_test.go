package file

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// process opens alice's index the way one backend pod does: its own handle,
// its own client of the lock service.
func process(t *testing.T, home string, locker locks.Locker) mailbox.UserIndex {
	t.Helper()
	idx := New(WithLocker(locker)).OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func inbox(t *testing.T, idx mailbox.UserIndex) *mailbox.Folder {
	t.Helper()
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func metaOf(t *testing.T, idx mailbox.UserIndex, folderID uint64, uid uint32) *mailbox.MessageMeta {
	t.Helper()
	msgs, err := idx.GetMessages(folderID, mailbox.SeqSet{{From: uid, To: uid}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("uid %d: %d messages, %v", uid, len(msgs), err)
	}
	return msgs[0]
}

// The UIDLs one POP3 session saves are what the next session, in another
// process, reads, locked or from its login snapshot.
func TestPOP3UIDLsOneProcessSavesAreWhatTheNextReads(t *testing.T) {
	dial := lockstest.Server(t)
	home := t.TempDir()
	a, b := process(t, home, dial()), process(t, home, dial())
	fa := inbox(t, a)

	empty, err := a.GetPOP3UIDLs(fa.ID)
	if err != nil || len(empty) != 0 {
		t.Fatalf("a folder no session saved for reads %v, %v", empty, err)
	}
	want := map[uint32]string{1: "uidl-one", 2: "uidl-two"}
	if err := a.SavePOP3UIDLs(fa.ID, want); err != nil {
		t.Fatal(err)
	}
	fb := inbox(t, b)
	for name, read := range map[string]func(uint64) (map[uint32]string, error){
		"locked": b.GetPOP3UIDLs,
		"snapshot": b.(interface {
			GetPOP3UIDLsUnlocked(uint64) (map[uint32]string, error)
		}).GetPOP3UIDLsUnlocked,
	} {
		got, err := read(fb.ID)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 2 || got[1] != "uidl-one" || got[2] != "uidl-two" {
			t.Errorf("%s read %v, want %v", name, got, want)
		}
	}

	// A later save replaces the set: a uid the session deleted is gone.
	if err := b.SavePOP3UIDLs(fb.ID, map[uint32]string{2: "uidl-two"}); err != nil {
		t.Fatal(err)
	}
	got, err := a.GetPOP3UIDLs(fa.ID)
	if err != nil || len(got) != 1 || got[2] != "uidl-two" {
		t.Errorf("after the second save the first process reads %v, %v", got, err)
	}
}

// Lines a reader cannot use are skipped, not fatal: one bad line costs that
// message its UIDL, not the session its login.
func TestPOP3UIDLLinesThatDoNotParseAreSkipped(t *testing.T) {
	home := t.TempDir()
	idx := process(t, home, nil)
	f := inbox(t, idx)
	if err := idx.SavePOP3UIDLs(f.ID, map[uint32]string{7: "keep"}); err != nil {
		t.Fatal(err)
	}
	var path string
	_ = filepath.WalkDir(home, func(p string, d os.DirEntry, _ error) error {
		if d != nil && d.Name() == "pop3.uidl" {
			path = p
		}
		return nil
	})
	if path == "" {
		t.Fatal("no pop3.uidl written")
	}
	raw, _ := os.ReadFile(path)
	raw = append(raw, []byte("no tab here\nnotanumber\tx\n9\tnine\n")...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := idx.GetPOP3UIDLs(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[7] != "keep" || got[9] != "nine" {
		t.Errorf("read %v, want 7 and 9 only", got)
	}
}

// The alt-tier mark follows the names an alt move reports, is seen by another
// process, survives a flag change, and comes off on the way back.
func TestTheAltTierMarkFollowsTheMovedNames(t *testing.T) {
	dial := lockstest.Server(t)
	home := t.TempDir()
	a, b := process(t, home, dial()), process(t, home, dial())
	f := inbox(t, a)
	for uid, mapUID := range map[uint32]uint32{1: 5, 2: 6} {
		if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10, MapUID: mapUID}); err != nil {
			t.Fatal(err)
		}
	}

	if err := a.SetAltTier(f.ID, []string{"5", "99"}, true); err != nil {
		t.Fatal(err)
	}
	fb := inbox(t, b)
	if !metaOf(t, b, fb.ID, 1).AltTier || metaOf(t, b, fb.ID, 2).AltTier {
		t.Fatal("the other process does not see uid 1 alone on alt")
	}
	if err := b.AddFlags(fb.ID, 1, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveFlags(fb.ID, 1, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	if !metaOf(t, a, f.ID, 1).AltTier {
		t.Error("a flag change took the alt-tier mark off")
	}
	// A repeat changes nothing, and nothing named is nothing done.
	if err := a.SetAltTier(f.ID, []string{"5"}, true); err != nil {
		t.Fatal(err)
	}
	if err := a.SetAltTier(f.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := a.SetAltTier(f.ID, []string{"5"}, false); err != nil {
		t.Fatal(err)
	}
	if metaOf(t, b, fb.ID, 1).AltTier {
		t.Error("the mark stayed after the move back")
	}
}

// RemoveFlags clears what it names and keeps the rest, flags and keywords alike.
func TestRemoveFlagsKeepsWhatItDoesNotName(t *testing.T) {
	idx := process(t, t.TempDir(), nil)
	f := inbox(t, idx)
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10, Flags: []string{`\Seen`, `\Flagged`}, Keywords: []string{"$Work", "$Later"}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.RemoveFlags(f.ID, 1, []string{`\Seen`}, []string{"$Later"}); err != nil {
		t.Fatal(err)
	}
	m := metaOf(t, idx, f.ID, 1)
	if !slices.Contains(m.Flags, `\Flagged`) || slices.Contains(m.Flags, `\Seen`) {
		t.Errorf("flags %v, want \\Flagged without \\Seen", m.Flags)
	}
	if len(m.Keywords) != 1 || m.Keywords[0] != "$Work" {
		t.Errorf("keywords %v, want [$Work]", m.Keywords)
	}
}

// Appends from two processes take uids and modseqs that climb across both:
// each append journals its modseq in the cycle that hands out its uid (#2398).
func TestAppendsFromTwoProcessesClimb(t *testing.T) {
	dial := lockstest.Server(t)
	home := t.TempDir()
	a, b := process(t, home, dial()), process(t, home, dial())
	fa, fb := inbox(t, a), inbox(t, b)
	var last mailbox.MessageMeta
	for i, p := range []struct {
		idx mailbox.UserIndex
		id  uint64
	}{{a, fa.ID}, {b, fb.ID}, {a, fa.ID}, {b, fb.ID}} {
		m := &mailbox.MessageMeta{Size: 10}
		if err := p.idx.AllocateAndAppend(p.id, m); err != nil {
			t.Fatal(err)
		}
		if i > 0 && (m.UID != last.UID+1 || m.ModSeq <= last.ModSeq) {
			t.Errorf("append %d took uid %d modseq %d after uid %d modseq %d", i, m.UID, m.ModSeq, last.UID, last.ModSeq)
		}
		last = *m
	}
}
