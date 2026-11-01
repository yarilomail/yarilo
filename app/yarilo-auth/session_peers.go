package main

import (
	"errors"
	"fmt"

	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/sockaddr"
)

// masterSessionPeers returns the uids that may SESSION on the master listener,
// or nil over TCP, where the internal TLS roles decide.
func masterSessionPeers(ac config.AuthServiceConfig) (map[uint32]bool, error) {
	if !sockaddr.IsUnix(ac.MasterListen) {
		return nil, nil
	}
	if !sockaddr.PeerSupported {
		return nil, errors.New("auth/master: this OS gives no peer credentials to check auth_service.master_session_users on a Unix socket; listen on TCP with internal TLS")
	}
	uids, err := sockaddr.UIDs(ac.MasterSessionUsers)
	if err != nil {
		return nil, fmt.Errorf("auth/master: auth_service.master_session_users: %w", err)
	}
	return uids, nil
}
