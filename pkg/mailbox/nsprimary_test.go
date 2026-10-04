package mailbox

import "testing"

func TestSubsFileFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shapes []NamespaceShape
		prefix []string
		want   []string
	}{
		{
			// The classic single personal namespace, prefixed or not, keeps
			// the file it has always had: no deployment's subscriptions move.
			name:   "one personal namespace with a prefix",
			shapes: []NamespaceShape{{Type: "personal"}},
			prefix: []string{"INBOX."},
			want:   []string{"subscriptions"},
		},
		{
			// mail_driver/mail_path on the INBOX namespace fold into a location;
			// it is still the INBOX namespace and keeps its name.
			name:   "the INBOX namespace with a location",
			shapes: []NamespaceShape{{Type: "personal", Location: "mdbox:~/mdbox", Inbox: true}},
			prefix: []string{""},
			want:   []string{"subscriptions"},
		},
		{
			name: "a private namespace with a store of its own",
			shapes: []NamespaceShape{
				{Type: "personal", Inbox: true},
				{Type: "shared", Location: "maildir:/var/mail/public"},
				{Type: "personal", Location: "virtual:%h/virtual"},
			},
			prefix: []string{"", "Public/", "Virtual/"},
			want:   []string{"subscriptions", "subscriptions-public", "subscriptions-virtual"},
		},
		{
			// The virtual one first: the INBOX namespace is chosen by inbox,
			// not by order, and the names follow that choice.
			name: "the INBOX namespace listed second",
			shapes: []NamespaceShape{
				{Type: "personal", Location: "virtual:%h/virtual"},
				{Type: "personal", Location: "mdbox:~/mdbox", Inbox: true},
			},
			prefix: []string{"Virtual/", ""},
			want:   []string{"subscriptions-virtual", "subscriptions"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]bool{}
			for i := range tc.shapes {
				got := SubsFileFor(tc.shapes, i, tc.prefix[i], "/")
				if got != tc.want[i] {
					t.Errorf("namespace %d (%q) uses %q, want %q", i, tc.prefix[i], got, tc.want[i])
				}
				if seen[got] {
					t.Errorf("two namespaces share %q", got)
				}
				seen[got] = true
			}
		})
	}
}

func TestPrimaryPersonalIndex(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shapes []NamespaceShape
		want   int
	}{
		{"marked inbox wins over order", []NamespaceShape{
			{Type: "personal", Location: "virtual:x"}, {Type: "personal", Location: "mdbox:y", Inbox: true}}, 1},
		{"else the first without a location", []NamespaceShape{
			{Type: "personal", Location: "virtual:x"}, {Type: "personal"}}, 1},
		{"else the first personal", []NamespaceShape{
			{Type: "shared"}, {Type: "personal", Location: "mdbox:y"}}, 1},
		{"no personal namespace", []NamespaceShape{{Type: "shared"}}, -1},
	} {
		if got := PrimaryPersonalIndex(tc.shapes); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
