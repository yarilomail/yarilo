package protocol

import (
	"reflect"
	"strings"
	"testing"
)

// A field put on the wire that no reader takes off it sends the session to the
// global mail location, silently (#1890).
func TestEveryFieldWrittenIsRead(t *testing.T) {
	full := &AuthResponse{
		Result: AuthOK, Username: "u@d.test",
		Home: "/h", MailLoc: "mdbox:/m", MailboxFormat: "mdbox",
		Groups: []string{"g1", "g2"}, ACLUser: "acl@d.test", ACLGroups: []string{"a1"},
		QuotaRules: []string{"*:storage=5G"}, QuotaOverFlag: "over", DirectorTag: "t1",
		VolatileDir: "/v", IndexDir: "/i", ControlDir: "/c", AltDir: "/alt",
		MailPath: "/mp", InboxPath: "/ip",
	}
	for _, f := range authOKFields {
		if f.get(full) == "" {
			t.Errorf("field %q is not populated by this row, so nothing about it is proven", f.key)
		}
	}

	back := &AuthResponse{Result: AuthOK, Username: full.Username}
	for _, tok := range AuthOKTokens(full) {
		if !ApplyAuthOKToken(back, tok) {
			t.Errorf("token %q was written and is not read", tok)
		}
	}
	if !reflect.DeepEqual(full, back) {
		t.Errorf("a round trip lost fields:\n wrote %+v\n read  %+v", full, back)
	}
}

// The chain emits the same field with a userdb_ prefix.
func TestAUserdbPrefixNamesTheSameField(t *testing.T) {
	res := &AuthResponse{}
	for _, tok := range []string{"userdb_home=/h", "userdb_acl_groups=a1,a2"} {
		if !ApplyAuthOKToken(res, tok) {
			t.Fatalf("%q was not recognised", tok)
		}
	}
	if res.Home != "/h" || strings.Join(res.ACLGroups, ",") != "a1,a2" {
		t.Errorf("prefixed fields did not land: %+v", res)
	}
}
