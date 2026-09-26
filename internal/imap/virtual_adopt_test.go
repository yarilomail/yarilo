package imap_test

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A mailbox whose header names its folders without identifying them, as one
// the reference wrote does: the first SELECT gives them the identity this
// server keeps them by, and the messages keep the uids they had (#1986).
func TestSelectTakesOverAHeaderThatOnlyNamesItsFolders(t *testing.T) {
	conn, rd := virtualServer(t, map[string]string{"All": "INBOX\n"}, seedFlagged)
	if got := existsCount(t, conn, rd, "a2", "Virtual/All"); got != 2 {
		t.Fatalf("EXISTS = %d, want the two seeded messages", got)
	}
	before := uidsOfSelected(t, conn, rd, "a3")

	// Strip the identities the way a header the reference wrote has none.
	idx := file.New().OpenUser(&mailbox.UserInfo{Username: "user@test.com", Home: lastVirtualHome + "/virtual"})
	f, err := idx.OpenFolder("All", 0)
	if err != nil {
		t.Fatal(err)
	}
	vi, ok := idx.(mailbox.VirtualIndexed)
	if !ok {
		t.Fatal("this index keeps no virtual header")
	}
	h, ok := vi.VirtualHeader(f.ID)
	if !ok || len(h.Backing) == 0 {
		t.Fatalf("header = %+v, want the folder the mailbox draws from", h)
	}
	named := h
	named.Backing = append(h.Backing[:0:0], h.Backing...)
	for i := range named.Backing {
		named.Backing[i].GUID = [16]byte{}
	}
	if err := vi.SetVirtualHeader(f.ID, named); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	other, ord := loginTo(t, lastVirtualAddr)
	if got := existsCount(t, other, ord, "b1", "Virtual/All"); got != 2 {
		t.Fatalf("EXISTS = %d after the takeover, want 2", got)
	}
	if after := uidsOfSelected(t, other, ord, "b2"); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("uids %v became %v: the takeover renumbered a migrated mailbox", before, after)
	}
}
