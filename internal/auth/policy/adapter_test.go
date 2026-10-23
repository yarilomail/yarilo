package policy

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// Every field the auth service fills reaches the client's request.
func TestTheAdapterCarriesEveryField(t *testing.T) {
	in := protocol.PolicyRequest{Username: "u", Password: "p", RemoteIP: "192.0.2.1", LocalIP: "198.51.100.2",
		Service: "imap", DeviceID: "d", SessionID: "s", TLS: true, FailType: "account"}
	want := Request{Username: "u", Password: "p", RemoteIP: "192.0.2.1", LocalIP: "198.51.100.2",
		Service: "imap", DeviceID: "d", SessionID: "s", TLS: true, FailType: "account"}
	if got := toRequest(in); got != want {
		t.Errorf("toRequest = %+v, want %+v", got, want)
	}
}
