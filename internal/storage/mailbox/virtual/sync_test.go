package virtual

import (
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
}

func (s *stubResolver) Folders(*Config) ([]Backing, error) { return s.folders, nil }

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
	got, err := Sync(cfg, mailbox.VirtualHeader{}, r)
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
	got, err := Sync(cfg, mailbox.VirtualHeader{}, r)
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
	got, err := Sync(cfg, was, r)
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
