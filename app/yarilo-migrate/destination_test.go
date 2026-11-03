package main

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox"
)

// --dst names a store the import can write, under either of sdbox's names,
// and the layout is keyed by the store's own name.
func TestTheDestinationStore(t *testing.T) {
	tests := []struct {
		dst, driver string
		check       func(any) bool
	}{
		{"sdbox", "sdbox", func(b any) bool { _, ok := b.(*dboxv2.Backend); return ok }},
		{"DBOX", "sdbox", func(b any) bool { _, ok := b.(*dboxv2.Backend); return ok }},
		{"mdbox", "mdbox", func(b any) bool { _, ok := b.(*mdbox.Backend); return ok }},
	}
	for _, tc := range tests {
		be, err := pickBackend(tc.dst)
		if err != nil || !tc.check(be) {
			t.Errorf("pickBackend(%q) = %T, %v", tc.dst, be, err)
		}
		if got := destinationDriver(tc.dst); got != tc.driver {
			t.Errorf("destinationDriver(%q) = %q, want %q", tc.dst, got, tc.driver)
		}
	}
	if _, err := pickBackend("maildir"); err == nil {
		t.Error("maildir was accepted as an import destination")
	}
	if got := destinationDriver("Maildir"); got != "maildir" {
		t.Errorf("destinationDriver(Maildir) = %q", got)
	}
}
