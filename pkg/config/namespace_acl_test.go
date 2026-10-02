package config

import (
	"strings"
	"testing"
)

const publicNS = `
  - type: shared
    prefix: "Public/"
    separator: "/"
    location: "maildir:/var/mail/public"
`

const ownerNS = `
  - type: shared
    prefix: "user/%u/"
    separator: "/"
    location: "maildir:%h/Maildir"
`

const otherNS = `
  - type: other
    prefix: "user/"
    separator: "/"
`

// A non-personal namespace with ACL off opens it to every user, so the loader
// refuses it and names the namespace and the key.
func TestSharedNamespaceNeedsACL(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		refuse     []string
	}{
		{name: "public with acl off refuses", yaml: personalNS + publicNS,
			refuse: []string{"namespace 1", "type shared", `"Public/"`, "acl.enabled"}},
		{name: "owner-templated with acl off refuses", yaml: personalNS + ownerNS,
			refuse: []string{"namespace 1", "type shared", `"user/%u/"`, "acl.enabled"}},
		{name: "other users with acl off refuses", yaml: personalNS + otherNS,
			refuse: []string{"namespace 1", "type other", `"user/"`, "acl.enabled"}},
		{name: "acl off stated explicitly refuses", yaml: personalNS + publicNS + "acl:\n  enabled: false\n",
			refuse: []string{`"Public/"`, "acl.enabled"}},
		{name: "public with acl on loads", yaml: personalNS + publicNS + "acl:\n  enabled: true\n"},
		{name: "owner-templated with acl on loads", yaml: personalNS + ownerNS + "acl:\n  enabled: true\n"},
		{name: "personal only with acl off loads", yaml: personalNS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			if tc.refuse == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("loaded; want a refusal")
			}
			for _, want := range tc.refuse {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %s", err, want)
				}
			}
		})
	}
}
