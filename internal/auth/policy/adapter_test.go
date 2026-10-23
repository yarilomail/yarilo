package policy

import (
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// Every field the auth service fills reaches the client's request.
func TestTheAdapterCarriesEveryField(t *testing.T) {
	vars, passdb, userdb := map[string]string{"mechanism": "PLAIN"}, map[string]string{"a": "1"}, map[string]string{"b": "2"}
	in := protocol.PolicyRequest{Username: "u", Password: "p", RemoteIP: "192.0.2.1", LocalIP: "198.51.100.2",
		Service: "imap", DeviceID: "d", SessionID: "s", TLS: true, FailType: "account",
		Vars: vars, Passdb: passdb, Userdb: userdb}
	want := Request{Username: "u", Password: "p", RemoteIP: "192.0.2.1", LocalIP: "198.51.100.2",
		Service: "imap", DeviceID: "d", SessionID: "s", TLS: true, FailType: "account",
		Vars: vars, Passdb: passdb, Userdb: userdb}
	if got := toRequest(in); !reflect.DeepEqual(got, want) {
		t.Errorf("toRequest = %+v, want %+v", got, want)
	}
}
