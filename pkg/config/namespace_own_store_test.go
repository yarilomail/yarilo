package config

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A second private namespace with a store of its own keeps its own
// subscriptions, so the loader accepts it, whatever order the set is in.
func TestValidateAcceptsAPrivateNamespaceWithItsOwnStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		nss  []NamespaceConfig
	}{
		{"the stand's set", []NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", Inbox: true},
			{Type: "shared", Prefix: "Public/", Separator: "/", Location: "maildir:/var/mail/public"},
			{Type: "personal", Prefix: "Virtual/", Separator: "/", Location: "virtual:%h/virtual"},
		}},
		{"the INBOX namespace listed second, with a location", []NamespaceConfig{
			{Type: "personal", Prefix: "Virtual/", Separator: "/", Location: "virtual:%h/virtual"},
			{Type: "personal", Prefix: "", Separator: "/", Location: "mdbox:~/mdbox", Inbox: true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateNamespaceTypes(tc.nss); err != nil {
				t.Errorf("rejected: %v", err)
			}
		})
	}
}

// Accepting is not enough: with the INBOX namespace chosen wrongly the names
// still differ, so the row reads which namespace keeps the bare file.
func TestNamespaceShapesFindTheInboxNamespace(t *testing.T) {
	nss := []NamespaceConfig{
		{Type: "personal", Prefix: "Virtual/", Separator: "/", Location: "virtual:%h/virtual"},
		{Type: "personal", Prefix: "", Separator: "/", Location: "mdbox:~/mdbox", Inbox: true},
	}
	shapes := NamespaceShapes(nss)
	want := []string{"subscriptions-virtual", "subscriptions"}
	for i, ns := range nss {
		if got := mailbox.SubsFileFor(shapes, i, ns.Prefix, ns.Separator); got != want[i] {
			t.Errorf("namespace %d (%q) keeps %q, want %q", i, ns.Prefix, got, want[i])
		}
	}
}
