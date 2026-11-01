package main

import (
	"os/user"
	"strconv"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/sockaddr"
)

func TestMasterSessionPeersApplyOnlyToAUnixListener(t *testing.T) {
	if !sockaddr.PeerSupported {
		t.Skip("no peer credentials on this OS")
	}
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	uid, _ := strconv.ParseUint(me.Uid, 10, 32)
	for _, tc := range []struct {
		name    string
		listen  string
		users   []string
		want    map[uint32]bool
		wantErr bool
	}{
		{"tcp is left to mTLS", "127.0.0.1:9102", []string{"no-such-user-yarilo-test"}, nil, false},
		{"unix resolves the name", "unix:/run/yarilo/auth/auth-master.sock", []string{me.Username}, map[uint32]bool{uint32(uid): true}, false},
		{"unix, unknown name", "unix:/run/yarilo/auth/auth-master.sock", []string{"no-such-user-yarilo-test"}, nil, true},
		{"unix, empty list lets none", "unix:/run/yarilo/auth/auth-master.sock", []string{}, map[uint32]bool{}, false},
	} {
		got, err := masterSessionPeers(config.AuthServiceConfig{MasterListen: tc.listen, MasterSessionUsers: tc.users})
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err %v, want error %v", tc.name, err, tc.wantErr)
			continue
		}
		if (got == nil) != (tc.want == nil) || len(got) != len(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
			continue
		}
		for u := range tc.want {
			if !got[u] {
				t.Errorf("%s: %v lacks %d", tc.name, got, u)
			}
		}
	}
}
