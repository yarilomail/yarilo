package protocol

import (
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/emersion/go-sasl"

	"github.com/yarilomail/yarilo/v2/pkg/sqlvars"
)

// Vars are the values a query may filter a lookup on besides the name (#2227).
func (r *Request) Vars() sqlvars.Vars {
	return sqlvars.Vars{Service: r.Service, RemoteIP: r.RemoteIP, LocalIP: r.LocalIP}
}

// UserdbFor is a Userdb whose answer can depend on the request.
type UserdbFor interface {
	LookupFor(username string, v sqlvars.Vars) (*UserInfo, error)
	// UsesRequest reports whether a lookup reads anything besides the name.
	UsesRequest() bool
}

// SCRAMSha256LookupFor is SCRAMSha256Lookup with the request's values.
type SCRAMSha256LookupFor interface {
	LookupSCRAMSha256For(username string, v sqlvars.Vars) (*sasl.ScramCredentials, error)
}

// SCRAMSha1LookupFor is SCRAMSha1Lookup with the request's values.
type SCRAMSha1LookupFor interface {
	LookupSCRAMSha1For(username string, v sqlvars.Vars) (*sasl.ScramCredentials, error)
}

// CredentialsLookupFor is CredentialsLookup with the request's values.
type CredentialsLookupFor interface {
	LookupCredentialsFor(username string, v sqlvars.Vars) (bool, error)
}

// LookupUserdb asks u for username, through LookupFor when u can use v.
func LookupUserdb(u Userdb, username string, v sqlvars.Vars) (*UserInfo, error) {
	if f, ok := u.(UserdbFor); ok {
		return f.LookupFor(username, v)
	}
	return u.Lookup(username)
}

// UserdbUsesRequest reports whether u's answer can depend on the request.
func UserdbUsesRequest(u Userdb) bool {
	f, ok := u.(UserdbFor)
	return ok && f.UsesRequest()
}

// LookupFor walks the chain like Lookup, each entry given v.
func (c UserdbChain) LookupFor(username string, v sqlvars.Vars) (*UserInfo, error) {
	for _, db := range c {
		ui, err := LookupUserdb(db, username, v)
		if err != nil {
			return nil, err
		}
		if ui != nil {
			return ui, nil
		}
	}
	return nil, nil
}

// UsesRequest is true when any entry's answer can depend on the request.
func (c UserdbChain) UsesRequest() bool {
	for _, db := range c {
		if UserdbUsesRequest(db) {
			return true
		}
	}
	return false
}

// AddressAware is a passdb whose answer can depend on the client's addresses.
type AddressAware interface {
	UsesAddresses() bool
}

// refuseByUserdb runs the userdb rule on an accepted login and answers a refusal;
// "" goes on. A userdb that reads no request is not asked (#2227).
func (s *Server) refuseByUserdb(conn net.Conn, id, sessionID, user string, v sqlvars.Vars, reject func(net.Conn, string)) string {
	if s.userdb == nil || !UserdbUsesRequest(s.userdb) {
		return ""
	}
	ui, err := LookupUserdb(s.userdb, user, v)
	if err != nil {
		if s.internalFailureDelay > 0 {
			time.Sleep(s.internalFailureDelay)
		}
		slog.Warn("auth: userdb lookup failed after login", "sid", sessionID, "proto", v.Service, "user", user, "err", err)
		fmt.Fprintf(conn, "FAIL\t%s\tcode=temp_fail\n", id)
		return "tempfail"
	}
	if ui != nil {
		return ""
	}
	// The same answer and delay as a wrong password (D4): a distinct one
	// would tell the client its credential was right.
	if s.failureDelay > 0 {
		time.Sleep(s.failureDelay)
	}
	slog.Info("auth: fail", "sid", sessionID, "proto", v.Service, "user", user, "result", "fail", "reason", "protocol")
	reject(conn, id)
	return "fail"
}
