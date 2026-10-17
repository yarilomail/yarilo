package protocol

import (
	"reflect"
	"testing"
)

// identity_aliases arrives space-separated from a SQL column, survives the
// wire to the backend, and keeps every address.
func TestIdentityAliasesReachTheBackend(t *testing.T) {
	ui := &UserInfo{}
	if err := AssignField(ui, "identity_aliases", "a@d.test b@d.test,c@d.test"); err != nil {
		t.Fatal(err)
	}
	want := []string{"a@d.test", "b@d.test", "c@d.test"}
	if !reflect.DeepEqual(ui.IdentityAliases, want) {
		t.Fatalf("parsed %q, want %q", ui.IdentityAliases, want)
	}
	var tokens []string
	ui.VisitFields(func(k, v string) { tokens = append(tokens, k+"="+v) })
	back, err := ParseUserInfo(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.IdentityAliases, want) {
		t.Errorf("after the wire %q, want %q (tokens %q)", back.IdentityAliases, want, tokens)
	}
}
