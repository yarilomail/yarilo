package virtual

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type stubResolver struct {
	folders []Backing
	// keep names the uids each folder contributes under its rule; a folder
	// absent from the map keeps everything.
	keep map[string]map[uint32]bool
	// rules records what rule each folder was asked about.
	rules map[string]string
	// reads counts the folders whose messages a pass read.
	reads map[string]int
}

// Folders answers the stamps only, as the session does; Messages is the read.
func (s *stubResolver) Folders(*Config) ([]Backing, error) {
	out := make([]Backing, len(s.folders))
	for i, b := range s.folders {
		b.Messages = nil
		out[i] = b
	}
	return out, nil
}

func (s *stubResolver) Messages(b Backing) ([]*mailbox.MessageMeta, error) {
	if s.reads == nil {
		s.reads = map[string]int{}
	}
	s.reads[b.Name]++
	for _, f := range s.folders {
		if f.GUID == b.GUID {
			return f.Messages, nil
		}
	}
	return nil, nil
}

func (s *stubResolver) Matches(b Backing, rule string) (map[uint32]bool, error) {
	if s.rules == nil {
		s.rules = map[string]string{}
	}
	s.rules[b.Name] = rule
	return s.keep[b.Name], nil
}

func msgs(uids ...uint32) []*mailbox.MessageMeta {
	out := make([]*mailbox.MessageMeta, 0, len(uids))
	for _, u := range uids {
		out = append(out, &mailbox.MessageMeta{UID: u, GUID: [16]byte{byte(u)}})
	}
	return out
}

// The rule decides membership at sync, so a message it does not keep is not in
// the mailbox at all -- EXISTS, not SEARCH, is what says so.
func TestTheRuleDecidesMembershipAtSync(t *testing.T) {
	cfg, err := ParseConfig(strings.NewReader("INBOX\n  unseen\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := &stubResolver{
		folders: []Backing{{Name: "INBOX", GUID: [16]byte{1}, UIDValidity: 1, Messages: msgs(1, 2, 3)}},
		keep:    map[string]map[uint32]bool{"INBOX": {2: true}},
	}
	got, err := Sync(cfg, mailbox.VirtualHeader{}, nil, Open, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 1 || got.Records[0].VirtualRealUID != 2 {
		t.Errorf("the mailbox holds %d messages, want only the one the rule keeps", len(got.Records))
	}
	if r.rules["INBOX"] != "unseen" {
		t.Errorf("the folder was asked about rule %q, want the one from the file", r.rules["INBOX"])
	}
}

// A copy in two backing folders is two messages here, each with its own flags:
// the virtual mailbox lists copies, the search index lists messages.
func TestACopyInTwoFoldersIsTwoMessages(t *testing.T) {
	cfg, err := ParseConfig(strings.NewReader("INBOX\nArchive\n"))
	if err != nil {
		t.Fatal(err)
	}
	shared := [16]byte{7}
	r := &stubResolver{folders: []Backing{
		{Name: "INBOX", GUID: [16]byte{1}, UIDValidity: 1, Messages: []*mailbox.MessageMeta{
			{UID: 11, GUID: shared, Flags: []string{`\Seen`}},
		}},
		{Name: "Archive", GUID: [16]byte{2}, UIDValidity: 1, Messages: []*mailbox.MessageMeta{
			{UID: 22, GUID: shared},
		}},
	}}
	got, err := Sync(cfg, mailbox.VirtualHeader{}, nil, Open, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 2 {
		t.Fatalf("one message in two folders produced %d records, want 2", len(got.Records))
	}
	if got.Records[0].VirtualBacking == got.Records[1].VirtualBacking {
		t.Error("both records name the same backing folder")
	}
	if len(got.Records[0].Flags) == len(got.Records[1].Flags) {
		t.Error("the two copies came back with the same flags, so one client's \\Seen would show on both")
	}
}

// A changed rule is a differently defined set: the pass starts from nothing
// rather than adding to what an older definition left.
func TestAChangedRuleRebuilds(t *testing.T) {
	cfg, err := ParseConfig(strings.NewReader("INBOX\n  unseen\n"))
	if err != nil {
		t.Fatal(err)
	}
	was := mailbox.VirtualHeader{SearchCRC32: cfg.SearchArgsCRC32 ^ 1, Backing: []mailbox.VirtualBacking{
		{ID: 9, GUID: [16]byte{42}, Name: "Gone"},
	}, HighestBackingID: 9}
	r := &stubResolver{folders: []Backing{{Name: "INBOX", GUID: [16]byte{1}, UIDValidity: 1, Messages: msgs(1)}}}
	got, err := Sync(cfg, was, nil, Open, r)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Rebuilt {
		t.Error("a changed rule did not rebuild")
	}
	if len(got.Header.Backing) != 1 || got.Header.Backing[0].Name != "INBOX" {
		t.Errorf("the rebuilt header still carries %+v", got.Header.Backing)
	}
}

// The negative line takes a folder out of the set, whatever else names it.
func TestNegativeLineExcludes(t *testing.T) {
	cfg, err := ParseConfig(strings.NewReader("Archive/*\n-Archive/2019\n"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"Archive/2026": true, "Archive/2019": false} {
		if got := Selects(cfg, name); got != want {
			t.Errorf("Selects(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPatternMatching(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"Archive/*", "Archive/2026/03", true},
		{"Archive/%", "Archive/2026", true},
		{"Archive/%", "Archive/2026/03", false},
		{"*", "anything/at/all", true},
		{"INBOX", "INBOX", true},
		{"INBOX", "INBOX/sub", false},
	} {
		if got := matchPattern(tc.pattern, tc.name); got != tc.want {
			t.Errorf("%q against %q = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// stamped is the header a pass leaves for these folders, as if it had read them.
func stamped(t *testing.T, cfg *Config, r *stubResolver) (mailbox.VirtualHeader, []*mailbox.MessageMeta) {
	t.Helper()
	got, err := Sync(cfg, mailbox.VirtualHeader{}, nil, Open, r)
	if err != nil {
		t.Fatal(err)
	}
	old := make([]*mailbox.MessageMeta, len(got.Records))
	for i := range got.Records {
		rec := got.Records[i]
		rec.UID, rec.ModSeq = uint32(i+1), uint64(10+i)
		old[i] = &rec
	}
	r.reads = nil
	return got.Header, old
}

func inbox(modseq uint64, m ...*mailbox.MessageMeta) Backing {
	return Backing{Name: "INBOX", GUID: [16]byte{1}, UIDValidity: 1, NextUID: 10, HighestModSeq: modseq, Messages: m}
}

func archive(modseq uint64, m ...*mailbox.MessageMeta) Backing {
	return Backing{Name: "Archive", GUID: [16]byte{2}, UIDValidity: 1, NextUID: 10, HighestModSeq: modseq, Messages: m}
}

// A folder whose state did not move is not read: an open mailbox syncs on
// every NOOP, and reading every folder each time is the cost that avoids.
func TestAnUnmovedFolderIsNotRead(t *testing.T) {
	cfg, _ := ParseConfig(strings.NewReader("INBOX\nArchive\n"))
	r := &stubResolver{folders: []Backing{inbox(5, msgs(1)...), archive(7, msgs(1)...)}}
	hdr, old := stamped(t, cfg, r)

	r.folders[1] = archive(8, msgs(1, 2)...) // only Archive moved
	got, err := Sync(cfg, hdr, old, Poll, r)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Changed || r.reads["INBOX"] != 0 || r.reads["Archive"] != 1 {
		t.Errorf("changed=%v reads=%v, want only Archive read", got.Changed, r.reads)
	}
	if len(got.Records) != 3 {
		t.Errorf("%d records, want INBOX's one carried and Archive's two", len(got.Records))
	}

	hdr, old = got.Header, nil
	for i := range got.Records {
		rec := got.Records[i]
		old = append(old, &rec)
	}
	r.reads = nil
	again, err := Sync(cfg, hdr, old, Poll, r)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed || len(r.reads) != 0 {
		t.Errorf("nothing moved, yet changed=%v reads=%v", again.Changed, r.reads)
	}
}

// A flag change on the copy is a change of the record: it takes a new modseq,
// so CHANGEDSINCE and the next poll report it (virtual-sync.c:66-92).
func TestAFlagChangeOnTheCopyMovesTheRecord(t *testing.T) {
	cfg, _ := ParseConfig(strings.NewReader("INBOX\n"))
	r := &stubResolver{folders: []Backing{inbox(5, msgs(1, 2)...)}}
	hdr, old := stamped(t, cfg, r)

	seen := msgs(1, 2)
	seen[0].Flags = []string{`\Seen`}
	r.folders[0] = inbox(6, seen...)
	got, err := Sync(cfg, hdr, old, Poll, r)
	if err != nil {
		t.Fatal(err)
	}
	by := map[uint32]mailbox.MessageMeta{}
	for _, m := range got.Records {
		by[m.VirtualRealUID] = m
	}
	if by[1].ModSeq != 0 || by[1].UID != 1 {
		t.Errorf("the changed copy's record is uid %d modseq %d, want its uid and a new modseq", by[1].UID, by[1].ModSeq)
	}
	if by[2].ModSeq != 11 {
		t.Errorf("the unchanged copy's record has modseq %d, want the 11 it had", by[2].ModSeq)
	}
}

// What stops matching stays while open and its folder is read again; what is
// gone leaves at once (virtual-sync.c:1092-1117, 1276-1278).
func TestAPollKeepsWhatStoppedMatchingAndDropsWhatIsGone(t *testing.T) {
	cfg, _ := ParseConfig(strings.NewReader("INBOX\n  unseen\n"))
	r := &stubResolver{
		folders: []Backing{inbox(5, msgs(1, 2, 3)...)},
		keep:    map[string]map[uint32]bool{"INBOX": {1: true, 2: true, 3: true}},
	}
	hdr, old := stamped(t, cfg, r)

	// 1 was read, 3 was expunged.
	r.folders[0] = inbox(6, msgs(1, 2)...)
	r.keep["INBOX"] = map[uint32]bool{2: true}
	for _, tc := range []struct {
		mode  Mode
		want  []uint32
		stamp uint64
	}{
		{Poll, []uint32{1, 2}, 0},
		{Open, []uint32{2}, 6},
	} {
		got, err := Sync(cfg, hdr, old, tc.mode, r)
		if err != nil {
			t.Fatal(err)
		}
		var real []uint32
		for _, m := range got.Records {
			real = append(real, m.VirtualRealUID)
		}
		if fmt.Sprint(real) != fmt.Sprint(tc.want) {
			t.Errorf("mode %d keeps copies %v, want %v", tc.mode, real, tc.want)
		}
		if s := got.Header.Backing[0].HighestModSeq; s != tc.stamp {
			t.Errorf("mode %d stamps modseq %d, want %d", tc.mode, s, tc.stamp)
		}
	}
}

// A recreated folder names other messages by the same uids: its records start
// over, and the other folder's keep theirs.
func TestANewUIDValidityRebuildsOnlyThatFolder(t *testing.T) {
	cfg, _ := ParseConfig(strings.NewReader("INBOX\nArchive\n"))
	r := &stubResolver{folders: []Backing{inbox(5, msgs(1)...), archive(7, msgs(1)...)}}
	hdr, old := stamped(t, cfg, r)

	recreated := inbox(1, msgs(1)...)
	recreated.UIDValidity = 2
	r.folders[0] = recreated
	got, err := Sync(cfg, hdr, old, Poll, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got.Records {
		switch m.VirtualBacking {
		case old[0].VirtualBacking:
			if m.UID != 0 {
				t.Errorf("the recreated folder's record kept uid %d", m.UID)
			}
		default:
			if m.UID != old[1].UID {
				t.Errorf("Archive's record has uid %d, want the %d it had", m.UID, old[1].UID)
			}
		}
	}
}

// The check is quiet when nothing moved and speaks when a folder did, left the
// set, or joined it; a folder that left must not make every later check speak.
func TestMovedSeesOnlyWhatChanged(t *testing.T) {
	cfg, _ := ParseConfig(strings.NewReader("INBOX\nArchive\n"))
	r := &stubResolver{folders: []Backing{inbox(5, msgs(1)...), archive(7, msgs(1)...)}}
	hdr, old := stamped(t, cfg, r)
	if moved, _ := Moved(cfg, hdr, r); moved {
		t.Error("nothing moved, yet the check spoke")
	}

	r.folders = r.folders[:1] // Archive left the set
	if moved, _ := Moved(cfg, hdr, r); !moved {
		t.Error("a folder left the set, yet the check was quiet")
	}
	got, err := Sync(cfg, hdr, old, Poll, r)
	if err != nil {
		t.Fatal(err)
	}
	if moved, _ := Moved(cfg, got.Header, r); moved {
		t.Error("after the pass that dropped Archive, the check still speaks")
	}

	r.folders[0] = inbox(6, msgs(1)...)
	if moved, _ := Moved(cfg, got.Header, r); !moved {
		t.Error("INBOX moved, yet the check was quiet")
	}
}
