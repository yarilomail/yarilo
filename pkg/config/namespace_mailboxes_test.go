package config

import (
	"maps"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

const personalNS = `
namespaces:
  - type: personal
    prefix: ""
    separator: "/"
    inbox: true
`

// The mailboxes block and the two delivery knobs, through the loader a pod
// uses: what is refused names the setting, what loads folds into one map.
func TestNamespaceMailboxesLoad(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		refuse     []string // substrings of the refusal; nil loads
		sentAttr   string
	}{
		{name: "same attribute in both places is one entry", yaml: personalNS + `
    mailboxes:
      Sent: { auto: subscribe, special_use: "\\Sent" }
`, sentAttr: `\Sent`},
		{name: "two attributes for one name refuse", yaml: personalNS + `
    mailboxes:
      Sent: { auto: subscribe, special_use: "\\Drafts" }
`, refuse: []string{`"Sent"`, `\Drafts`, `\Sent`}},
		{name: "a name only in mailboxes joins the map", yaml: personalNS + `
    mailboxes:
      Receipts: { auto: create, special_use: "\\Archive" }
`, sentAttr: `\Sent`},
		{name: "special_use outside the personal namespace refuses", yaml: personalNS + `
  - type: shared
    prefix: "Public/"
    separator: "/"
    location: "maildir:/var/mail/public"
    mailboxes:
      Board: { special_use: "\\Archive" }
`, refuse: []string{`"Public/"`, `"Board"`, "personal namespace only"}},
		{name: "auto in a shared namespace with its own store loads", yaml: personalNS + `
  - type: shared
    prefix: "Public/"
    separator: "/"
    location: "maildir:/var/mail/public"
    mailboxes:
      Board: { auto: subscribe }
acl:
  enabled: true
`, sentAttr: `\Sent`},
		{name: "auto in an owner-templated namespace refuses", yaml: personalNS + `
  - type: shared
    prefix: "user/%u/"
    separator: "/"
    location: "maildir:%h/Maildir"
    mailboxes:
      Board: { auto: create }
`, refuse: []string{`"Board"`, "no store of its own"}},
		{name: "auto in a virtual namespace refuses", yaml: personalNS + `
  - type: personal
    prefix: "Virtual/"
    separator: "/"
    location: "virtual:/etc/yarilo/virtual:INDEX=%h/index/virtual"
    mailboxes:
      All: { auto: create }
`, refuse: []string{`"All"`, "virtual"}},
		{name: "an unknown auto refuses", yaml: personalNS + `
    mailboxes:
      Sent: { auto: always }
`, refuse: []string{`"always"`, "no, create and subscribe"}},
		{name: "an unknown attribute refuses", yaml: personalNS + `
    mailboxes:
      Sent: { special_use: "\\Outbox" }
`, refuse: []string{`\Outbox`}},
		{name: "autosubscribe alone refuses", yaml: personalNS + `
protocol:
  lmtp:
    lda_mailbox_autosubscribe: true
`, refuse: []string{"lda_mailbox_autosubscribe", "lda_mailbox_autocreate"}},
		{name: "autocreate with autosubscribe loads", yaml: personalNS + `
protocol:
  lmtp:
    lda_mailbox_autocreate: true
    lda_mailbox_autosubscribe: true
`, sentAttr: `\Sent`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tc.yaml)
			if tc.refuse != nil {
				if err == nil {
					t.Fatal("loaded; want a refusal")
				}
				for _, want := range tc.refuse {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("refusal %q does not name %s", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got := cfg.Protocol.IMAP.SpecialUseDefaults["Sent"]; got != tc.sentAttr {
				t.Fatalf("Sent maps to %q, want %q", got, tc.sentAttr)
			}
			if strings.Contains(tc.yaml, "Receipts") && cfg.Protocol.IMAP.SpecialUseDefaults["Receipts"] != `\Archive` {
				t.Fatalf("Receipts is not in the folded map: %v", cfg.Protocol.IMAP.SpecialUseDefaults)
			}
		})
	}
}

// The namespace owning INBOX that names no mailboxes takes the default set,
// as a server with no namespaces does; a block of its own, even an empty one,
// replaces the set, and no other namespace takes it.
func TestTheDefaultMailboxes(t *testing.T) {
	def := mailbox.DefaultMailboxes()
	for _, tc := range []struct {
		name, yaml string
		want       map[string]mailbox.AutoMailbox
	}{
		{"no namespaces", "", def},
		{"a personal namespace without a block", personalNS, def},
		{"a block of its own", personalNS + `
    mailboxes:
      Receipts: { auto: subscribe }
`, map[string]mailbox.AutoMailbox{"Receipts": {Auto: mailbox.AutoSubscribe}}},
		{"an empty block", personalNS + `
    mailboxes: {}
`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tc.yaml)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.PersonalAutoMailboxes(); !maps.Equal(got, tc.want) {
				t.Errorf("mailboxes %v, want %v", got, tc.want)
			}
		})
	}
	cfg, err := loadYAML(t, personalNS+`
  - type: personal
    prefix: "Virtual/"
    separator: "/"
    location: "virtual:/etc/yarilo/virtual:INDEX=%h/index/virtual"
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Namespaces[1].AutoMailboxes(); got != nil {
		t.Errorf("the virtual namespace took %v, want none", got)
	}
}

// The set is the four special-use folders, each made, subscribed and marked.
func TestTheDefaultSetIsTheFourSpecialUseFolders(t *testing.T) {
	want := map[string]string{"Sent": `\Sent`, "Drafts": `\Drafts`, "Trash": `\Trash`, "Junk": `\Junk`}
	got := mailbox.DefaultMailboxes()
	if len(got) != len(want) {
		t.Fatalf("set %v, want %v", got, want)
	}
	for name, attr := range want {
		if mb := got[name]; mb.Auto != mailbox.AutoSubscribe || mb.SpecialUse != attr {
			t.Errorf("%s = %+v, want subscribe %s", name, mb, attr)
		}
	}
}
