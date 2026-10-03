package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
)

// RoleSuffix ends the one DNS SAN that names a certificate's role:
// "<role>.role.yarilo.internal". Other DNS SANs are not identity.
const RoleSuffix = ".role.yarilo.internal"

// Role is the component a certificate speaks for: one per binary, plus admin
// for the operator tools.
type Role string

const (
	RoleAuth             Role = "auth"
	RoleWarden           Role = "warden"
	RoleLocks            Role = "locks"
	RoleDict             Role = "dict"
	RoleDirector         Role = "director"
	RoleFTS              Role = "fts"
	RoleBackendAPI       Role = "backend-api"
	RoleBackendReg       Role = "backend-reg"
	RoleIMAP             Role = "imap"
	RolePOP3             Role = "pop3"
	RoleLMTP             Role = "lmtp"
	RoleManageSieve      Role = "managesieve"
	RoleSubmission       Role = "submission"
	RoleJMAP             Role = "jmap"
	RoleIMAPLogin        Role = "imap-login"
	RolePOP3Login        Role = "pop3-login"
	RoleSubmissionLogin  Role = "submission-login"
	RoleManageSieveLogin Role = "managesieve-login"
	RoleLMTPLogin        Role = "lmtp-login"
	RoleJMAPLogin        Role = "jmap-login"
	RoleSASLLogin        Role = "sasl-login"
	RoleQuotaStatus      Role = "quota-status"
	RoleAdmin            Role = "admin"
)

// Roles is every role a certificate may carry.
var Roles = []Role{
	RoleAuth, RoleWarden, RoleLocks, RoleDict, RoleDirector, RoleFTS, RoleBackendAPI, RoleBackendReg,
	RoleIMAP, RolePOP3, RoleLMTP, RoleManageSieve, RoleSubmission, RoleJMAP,
	RoleIMAPLogin, RolePOP3Login, RoleSubmissionLogin, RoleManageSieveLogin, RoleLMTPLogin, RoleJMAPLogin,
	RoleSASLLogin, RoleQuotaStatus, RoleAdmin,
}

// Listener names an internal server socket; the allow-list is keyed by it.
type Listener string

const (
	ListenerAuthClient    Listener = "auth-client"
	ListenerAuthMaster    Listener = "auth-master"
	ListenerWarden        Listener = "warden"
	ListenerLocks         Listener = "locks"
	ListenerDict          Listener = "dict"
	ListenerDirector      Listener = "director"
	ListenerDirectorAPI   Listener = "director-api"
	ListenerBackendAPI    Listener = "backend-api"
	ListenerIMAPBackend   Listener = "imap-backend"
	ListenerPOP3Backend   Listener = "pop3-backend"
	ListenerLMTPBackend   Listener = "lmtp-backend"
	ListenerSieveBackend  Listener = "managesieve-backend"
	ListenerSubmitBackend Listener = "submission-backend"
	ListenerJMAPBackend   Listener = "jmap-backend"
	ListenerFTS           Listener = "fts"
)

// Listeners is every internal listener.
var Listeners = []Listener{
	ListenerAuthClient, ListenerAuthMaster, ListenerWarden, ListenerLocks, ListenerDict, ListenerDirector,
	ListenerDirectorAPI, ListenerBackendAPI, ListenerIMAPBackend, ListenerPOP3Backend, ListenerLMTPBackend,
	ListenerSieveBackend, ListenerSubmitBackend, ListenerJMAPBackend, ListenerFTS,
}

var sessions = []Role{RoleIMAP, RolePOP3, RoleLMTP, RoleManageSieve}

// allowed is who may call whom: a property of the protocols, not of a
// deployment, so it has no configuration key.
var allowed = map[Listener][]Role{
	ListenerAuthClient: append([]Role{RoleIMAPLogin, RolePOP3Login, RoleSubmissionLogin, RoleManageSieveLogin,
		RoleJMAPLogin, RoleSASLLogin, RoleSubmission, RoleAdmin}, sessions...),
	ListenerAuthMaster: append([]Role{RoleBackendAPI, RoleFTS, RoleJMAP, RoleQuotaStatus, RoleLMTPLogin, RoleAdmin}, sessions...),
	ListenerWarden: {RoleAuth, RoleIMAPLogin, RolePOP3Login, RoleSubmissionLogin, RoleManageSieveLogin,
		RoleLMTPLogin, RoleJMAPLogin, RoleBackendAPI, RoleIMAP},
	ListenerLocks: append([]Role{RoleBackendAPI, RoleFTS, RoleJMAP, RoleAdmin}, sessions...),
	ListenerDict:  sessions,
	ListenerDirector: {RoleDirector, RoleIMAPLogin, RolePOP3Login, RoleSubmissionLogin, RoleManageSieveLogin,
		RoleLMTPLogin, RoleJMAPLogin, RoleBackendAPI, RoleBackendReg},
	ListenerDirectorAPI:   {RoleAdmin},
	ListenerBackendAPI:    {RoleAdmin, RoleBackendAPI},
	ListenerIMAPBackend:   {RoleIMAPLogin},
	ListenerPOP3Backend:   {RolePOP3Login},
	ListenerLMTPBackend:   {RoleLMTPLogin},
	ListenerSieveBackend:  {RoleManageSieveLogin},
	ListenerSubmitBackend: {RoleSubmissionLogin},
	ListenerJMAPBackend:   {RoleJMAPLogin},
	ListenerFTS:           append([]Role{RoleBackendAPI, RoleJMAP}, sessions...),
}

var logins = []Role{RoleIMAPLogin, RolePOP3Login, RoleSubmissionLogin, RoleManageSieveLogin, RoleLMTPLogin, RoleJMAPLogin}

// directorCommands narrows the director port per command; a command not listed
// is open to every role the port accepts.
var directorCommands = map[string][]Role{
	"DIRECTOR-JOIN":       {RoleDirector},
	"PEER":                {RoleDirector},
	"LOOKUP":              append([]Role{RoleBackendAPI}, logins...),
	"SESSION-OPEN":        logins,
	"SESSION-SYNC-START":  logins,
	"SESSION-SYNC":        logins,
	"SESSION-SYNC-END":    logins,
	"SESSION-CLOSE":       logins,
	"BACKEND-UNREACHABLE": logins,
	"BACKEND-UP":          {RoleBackendReg},
	"BACKEND-DOWN":        {RoleBackendReg},
	"BACKEND-FLUSH":       {RoleBackendReg},
	"HOST-REMOVE":         {RoleDirector},
	"USER-MOVE":           {RoleDirector},
	"USER-WEAK":           {RoleDirector},
	"USER-KICK":           {RoleDirector},
	"USER-KILLED":         {RoleDirector},
}

// DirectorCommandAllowed reports whether a peer of role r may send cmd on the director port.
func DirectorCommandAllowed(cmd string, r Role) bool {
	want, listed := directorCommands[cmd]
	return !listed || slices.Contains(want, r)
}

// Allowed is the roles a listener accepts.
func Allowed(l Listener) []Role { return slices.Clone(allowed[l]) }

var (
	errNoRole      = errors.New("no role SAN")
	errForeignRole = errors.New("foreign role")
)

// RoleOf reads the certificate's role. errNoRole: none; errForeignRole: two
// role SANs, or a label that is not a role.
func RoleOf(cert *x509.Certificate) (Role, error) {
	var found []string
	for _, name := range cert.DNSNames {
		if label, ok := strings.CutSuffix(strings.ToLower(name), RoleSuffix); ok {
			found = append(found, label)
		}
	}
	switch {
	case len(found) == 0:
		return "", errNoRole
	case len(found) > 1:
		return "", fmt.Errorf("%w: %d role SANs", errForeignRole, len(found))
	case !slices.Contains(Roles, Role(found[0])):
		return "", fmt.Errorf("%w: %q is not a role", errForeignRole, found[0])
	}
	return Role(found[0]), nil
}

// PeerRole is the role of a verified connection's client, for checks finer
// than the handshake; false when it carries none.
func PeerRole(cs tls.ConnectionState) (Role, bool) {
	if len(cs.PeerCertificates) == 0 {
		return "", false
	}
	r, err := RoleOf(cs.PeerCertificates[0])
	return r, err == nil
}

// roleless remembers which certificates without a role were already logged,
// so a pooled client does not repeat the line on every dial.
var roleless sync.Map

// verifyRole is the handshake check. A certificate without a role is accepted
// this release, logged once per listener and certificate; a foreign role is refused.
func verifyRole(l Listener) func(tls.ConnectionState) error {
	want := allowed[l]
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return nil // RequireAndVerifyClientCert already refused it
		}
		cert := cs.PeerCertificates[0]
		role, err := RoleOf(cert)
		if errors.Is(err, errNoRole) {
			if _, seen := roleless.LoadOrStore(string(l)+"\x00"+cert.SerialNumber.String(), true); !seen {
				slog.Warn("mtls: peer certificate carries no role; accepted this release, refused in the next",
					peerAttrs(l, cert, want)...)
			}
			return nil
		}
		if err == nil && slices.Contains(want, role) {
			return nil
		}
		reason := fmt.Sprintf("role %q not accepted here", role)
		if err != nil {
			reason = err.Error()
		}
		slog.Error("mtls: peer refused", append(peerAttrs(l, cert, want), "reason", reason)...)
		return fmt.Errorf("mtls: %s refuses %s: %s", l, cert.Subject, reason)
	}
}

func peerAttrs(l Listener, cert *x509.Certificate, want []Role) []any {
	var roleSANs []string
	for _, name := range cert.DNSNames {
		if strings.HasSuffix(strings.ToLower(name), RoleSuffix) {
			roleSANs = append(roleSANs, name)
		}
	}
	return []any{"listener", string(l), "subject", cert.Subject.String(), "serial", cert.SerialNumber.String(),
		"role_sans", roleSANs, "expected", want}
}

// WarnRolesUnchecked is the startup line of listeners running without
// internal TLS: nothing says who connects.
func WarnRolesUnchecked(ls ...Listener) {
	slog.Warn("mtls: internal TLS is off; peer roles are not checked", "listeners", ls)
}
