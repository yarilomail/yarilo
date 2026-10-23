package protocol

import (
	"strconv"
	"strings"

	"github.com/yarilomail/yarilo/pkg/sqlvars"
)

// policyVars is the variable table a policy template expands against: the
// reference's auth request table, by its names.
type policyVars struct {
	user, original, password, service string
	rip, lip, session, mechanism      string
	passdbID, cpid                    int
	cc                                clientConn
}

func (v policyVars) table() map[string]string {
	secured := ""
	switch v.cc.secured {
	case "tls":
		secured = "TLS"
	case "secured":
		secured = "secured"
	}
	id := ""
	if v.passdbID > 0 {
		id = strconv.Itoa(v.passdbID)
	}
	cert := ""
	if v.cc.validCert {
		cert = "valid"
	}
	domainFirst, domainLast := "", ""
	if _, after, ok := strings.Cut(v.user, "@"); ok {
		domainFirst, _, _ = strings.Cut(after, "@")
		domainLast = v.user[strings.LastIndexByte(v.user, '@')+1:]
	}
	lport, rport := v.cc.field("lport", "0"), v.cc.field("rport", "0")
	return map[string]string{
		"user":                      v.user,
		"protocol":                  sqlvars.ProtocolName(v.service),
		"home":                      "",
		"local_ip":                  v.lip,
		"remote_ip":                 v.rip,
		"client_pid":                strconv.Itoa(v.cpid),
		"password":                  v.password,
		"id":                        id,
		"mechanism":                 v.mechanism,
		"secured":                   secured,
		"local_port":                lport,
		"remote_port":               rport,
		"cert":                      cert,
		"login_user":                "",
		"session":                   v.session,
		"real_local_ip":             v.cc.field("real_lip", v.lip),
		"real_remote_ip":            v.cc.field("real_rip", v.rip),
		"real_local_port":           v.cc.field("real_lport", lport),
		"real_remote_port":          v.cc.field("real_rport", rport),
		"domain_first":              domainFirst,
		"domain_last":               domainLast,
		"master_user":               "",
		"session_pid":               "",
		"original_user":             v.original,
		"auth_user":                 v.original,
		"local_name":                v.cc.field("local_name", ""),
		"client_id":                 v.cc.clientID,
		"ssl_ja3_hash":              v.cc.field("ssl_ja3_hash", ""),
		"ssl_client_cert_fp":        v.cc.field("ssl_client_cert_fp", ""),
		"ssl_client_cert_pubkey_fp": v.cc.field("ssl_client_cert_pubkey_fp", ""),
		"owner_user":                v.user,
	}
}

// fieldMaps splits a chain's fields into the passdb's and the userdb's, for
// the passdb: and userdb: providers.
func fieldMaps(f *Fields) (passdb, userdb map[string]string) {
	passdb, userdb = map[string]string{}, map[string]string{}
	if f == nil {
		return passdb, userdb
	}
	f.Each(func(key, value string) bool {
		if name, ok := strings.CutPrefix(key, "userdb_"); ok {
			userdb[name] = value
		} else {
			passdb[key] = value
		}
		return true
	})
	return passdb, userdb
}

// policyAfterChain gives the policy what the chain learnt: the user it
// resolved, the passdb that answered and the fields it left.
func (s *Server) policyAfterChain(req *PolicyRequest, v policyVars, res *AuthResponse) {
	if res.Username != "" {
		v.user = res.Username
	}
	v.passdbID = res.PassdbID
	req.Vars = v.table()
	req.Passdb, req.Userdb = fieldMaps(res.Fields)
}
