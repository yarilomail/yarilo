//go:build flatcurve

package ftsservice

import (
	"sort"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/fts/language"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/internal/storage/search"
	"github.com/yarilomail/yarilo/v2/pkg/ftsproto"
)

// A subject search is a substring match whatever the index holds: a token
// below the minimum term size never reaches the index, so the word leaves the
// query and the scan proves the answer. Before, "-7" found nothing and "-1"
// returned its neighbour through the prefix "1*" (#2454).
func TestASubjectWithAOneCharacterTokenIsSearchedAsASubstring(t *testing.T) {
	svc, box, uidx := newTestService(t)
	for uid, subject := range map[uint32]string{1: "xtok-1793573171-7", 2: "xtok-1793573171-1", 3: "xtok-1793573171-77"} {
		saveRawMessage(t, box, uidx, uid, "From: a@test.com\r\nSubject: "+subject+"\r\n\r\nshort token\r\n")
	}
	if err := svc.Index(testUser, testMbox, 3, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 3)

	set := language.DefaultSettings()
	chain, err := language.NewMultiChain([]string{set.Language}, set.Filters, nil, set.TokenMaxLen, set.AddressMaxLen, 0)
	if err != nil {
		t.Fatal(err)
	}
	opts := search.Options{Client: ftsproto.Embedded{Service: svc}, Chain: chain, Enabled: true}
	f, err := uidx.OpenFolder(testMbox.Name, testMbox.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := uidx.GetMessages(f.ID, nil)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("messages: %d, %v", len(msgs), err)
	}
	for _, tc := range []struct {
		name string
		rule string
		want []uint32
	}{
		{"a one-character token finds every subject holding the string", "SUBJECT xtok-1793573171-7", []uint32{1, 3}},
		{"a one-character token does not return the neighbour", "SUBJECT xtok-1793573171-1", []uint32{2}},
		{"a two-character token is answered by the index", "SUBJECT xtok-1793573171-77", []uint32{3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keep, err := opts.Folder(mailboxbase.Open(box, uidx), testUser, f, msgs, tc.rule)
			if err != nil {
				t.Fatal(err)
			}
			var got []uint32
			for uid := range keep {
				got = append(got, uid)
			}
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			if len(got) != len(tc.want) || (len(got) > 0 && !equalUIDs(got, tc.want)) {
				t.Errorf("%s = %v, want %v", tc.rule, got, tc.want)
			}
		})
	}
}

func equalUIDs(a, b []uint32) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
