package sockaddr

import (
	"net"
	"os"
	"os/user"
	"strconv"
	"testing"
)

// acceptOne returns the server end of one connection to a fresh listener on network.
func acceptOne(t *testing.T, network string) net.Conn {
	t.Helper()
	addr := "127.0.0.1:0"
	if network == "unix" {
		addr = sockPath(t)
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	client, err := net.Dial(network, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return server
}

func TestPeerUIDIsTheConnectingProcess(t *testing.T) {
	if !PeerSupported {
		t.Skip("no peer credentials on this OS")
	}
	for _, tc := range []struct {
		network string
		wantErr bool
	}{
		{"unix", false},
		{"tcp", true},
	} {
		uid, err := PeerUID(acceptOne(t, tc.network))
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: PeerUID gave uid %d, want an error", tc.network, uid)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.network, err)
		}
		if uid != uint32(os.Getuid()) {
			t.Errorf("%s: peer uid %d, want this process's %d", tc.network, uid, os.Getuid())
		}
	}
}

func TestUIDsResolvesNamesAndNumbers(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	myUID, _ := strconv.ParseUint(me.Uid, 10, 32)
	for _, tc := range []struct {
		name    string
		in      []string
		want    []uint32
		wantErr bool
	}{
		{"number", []string{"4242"}, []uint32{4242}, false},
		{"name", []string{me.Username}, []uint32{uint32(myUID)}, false},
		{"unknown name", []string{"no-such-user-yarilo-test"}, nil, true},
		{"empty", nil, nil, false},
	} {
		got, err := UIDs(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: no error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
		for _, u := range tc.want {
			if !got[u] {
				t.Errorf("%s: %v lacks %d", tc.name, got, u)
			}
		}
	}
}
