package mtls_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"slices"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mtls"
	"github.com/yarilomail/yarilo/pkg/mtls/mtlstest"
)

func parseCert(t *testing.T, file string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(raw)
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRoleOf(t *testing.T) {
	ca := mtlstest.NewCA(t)
	for _, tc := range []struct {
		name  string
		sans  []string
		want  mtls.Role
		valid bool
	}{
		{name: "one role", sans: []string{"imap-login.role.yarilo.internal"}, want: mtls.RoleIMAPLogin, valid: true},
		{name: "other DNS SANs are not identity", sans: []string{"auth.yarilo.internal", "admin.example", "warden.role.yarilo.internal"}, want: mtls.RoleWarden, valid: true},
		{name: "upper case suffix", sans: []string{"Admin.Role.Yarilo.Internal"}, want: mtls.RoleAdmin, valid: true},
		{name: "none", sans: nil},
		{name: "two roles", sans: []string{"auth.role.yarilo.internal", "warden.role.yarilo.internal"}},
		{name: "unknown label", sans: []string{"root.role.yarilo.internal"}},
		{name: "suffix without the dot", sans: []string{"authrole.yarilo.internal"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cf, _ := ca.Issue(t, "leaf", tc.sans...)
			got, err := mtls.RoleOf(parseCert(t, cf))
			if (err == nil) != tc.valid || got != tc.want {
				t.Fatalf("RoleOf = %q, %v; want %q, valid %v", got, err, tc.want, tc.valid)
			}
		})
	}
}

// The matrix of #2132, spelled out so an edit to the table has to edit this too.
var wantAllowed = map[mtls.Listener][]mtls.Role{
	mtls.ListenerAuthClient: {"imap-login", "pop3-login", "submission-login", "managesieve-login", "jmap-login",
		"sasl-login", "submission", "admin", "imap", "pop3", "lmtp", "managesieve"},
	mtls.ListenerAuthMaster: {"backend-api", "fts", "jmap", "quota-status", "lmtp-login", "admin", "imap", "pop3", "lmtp", "managesieve"},
	mtls.ListenerWarden: {"auth", "imap-login", "pop3-login", "submission-login", "managesieve-login",
		"lmtp-login", "jmap-login", "backend-api", "imap"},
	mtls.ListenerLocks: {"backend-api", "fts", "jmap", "admin", "imap", "pop3", "lmtp", "managesieve"},
	mtls.ListenerDict:  {"imap", "pop3", "lmtp", "managesieve"},
	mtls.ListenerDirector: {"director", "imap-login", "pop3-login", "submission-login", "managesieve-login",
		"lmtp-login", "jmap-login", "backend-api", "backend-reg"},
	mtls.ListenerDirectorAPI:   {"admin", "director-admin"},
	mtls.ListenerBackendAPI:    {"admin", "backend-api"},
	mtls.ListenerIMAPBackend:   {"imap-login"},
	mtls.ListenerPOP3Backend:   {"pop3-login"},
	mtls.ListenerLMTPBackend:   {"lmtp-login"},
	mtls.ListenerSieveBackend:  {"managesieve-login"},
	mtls.ListenerSubmitBackend: {"submission-login"},
	mtls.ListenerJMAPBackend:   {"jmap-login"},
	mtls.ListenerFTS:           {"backend-api", "jmap", "imap", "pop3", "lmtp", "managesieve"},
}

func TestAllowListIsTheMatrix(t *testing.T) {
	if len(mtls.Listeners) != len(wantAllowed) {
		t.Fatalf("%d listeners, the matrix has %d", len(mtls.Listeners), len(wantAllowed))
	}
	for _, l := range mtls.Listeners {
		got, want := mtls.Allowed(l), slices.Clone(wantAllowed[l])
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s accepts %v, the matrix says %v", l, got, want)
		}
	}
}

// handshake runs one internal mTLS handshake and reports whether the server took it.
func handshake(t *testing.T, ca *mtlstest.CA, srvCert, srvKey string, l mtls.Listener, cliCert, cliKey string) bool {
	t.Helper()
	srvCfg, err := mtls.ServerConfig(srvCert, srvKey, ca.CAFile, l)
	if err != nil {
		t.Fatal(err)
	}
	cliCfg, err := mtls.ClientConfig(cliCert, cliKey, ca.CAFile, mtlstest.ServerName, -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan error, 1)
	go func() {
		srv := tls.Server(a, srvCfg)
		err := srv.Handshake()
		if err == nil {
			// The client learns of a refusal only on its first read in TLS 1.3.
			_, err = srv.Write([]byte{0})
		}
		done <- err
		a.Close()
	}()
	cli := tls.Client(b, cliCfg)
	if err := cli.Handshake(); err == nil {
		_, _ = cli.Read(make([]byte, 1))
	}
	return <-done == nil
}

func TestEveryListenerAcceptsOnlyItsRoles(t *testing.T) {
	ca := mtlstest.NewCA(t)
	srvCert, srvKey := ca.Role(t, mtls.RoleAuth)
	certs := map[mtls.Role][2]string{}
	for _, r := range mtls.Roles {
		c, k := ca.Role(t, r)
		certs[r] = [2]string{c, k}
	}
	noRoleC, noRoleK := ca.Issue(t, "shared")
	foreignC, foreignK := ca.Issue(t, "foreign", "root.role.yarilo.internal")
	twoC, twoK := ca.Issue(t, "two", "admin.role.yarilo.internal", "auth.role.yarilo.internal")

	for _, l := range mtls.Listeners {
		for _, r := range mtls.Roles {
			want := slices.Contains(wantAllowed[l], r)
			if got := handshake(t, ca, srvCert, srvKey, l, certs[r][0], certs[r][1]); got != want {
				t.Errorf("%s with role %s: accepted %v, want %v", l, r, got, want)
			}
		}
		if !handshake(t, ca, srvCert, srvKey, l, noRoleC, noRoleK) {
			t.Errorf("%s refused a certificate without a role; this release accepts it", l)
		}
		if handshake(t, ca, srvCert, srvKey, l, foreignC, foreignK) {
			t.Errorf("%s accepted an unknown role label", l)
		}
		if handshake(t, ca, srvCert, srvKey, l, twoC, twoK) {
			t.Errorf("%s accepted a certificate with two roles", l)
		}
	}
}

func TestServerConfigRefusesAnUnknownListener(t *testing.T) {
	ca := mtlstest.NewCA(t)
	c, k := ca.Role(t, mtls.RoleAuth)
	if _, err := mtls.ServerConfig(c, k, ca.CAFile, "nowhere"); err == nil {
		t.Fatal("an unknown listener got a config")
	}
}

func TestDirectorCommandAllowed(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		role mtls.Role
		want bool
	}{
		{"PEER", mtls.RoleDirector, true},
		{"PEER", mtls.RoleIMAPLogin, false},
		{"DIRECTOR-JOIN", mtls.RoleBackendReg, false},
		{"LOOKUP", mtls.RoleIMAPLogin, true},
		{"LOOKUP", mtls.RoleBackendAPI, true},
		{"LOOKUP", mtls.RoleBackendReg, false},
		{"SESSION-OPEN", mtls.RoleLMTPLogin, true},
		{"SESSION-OPEN", mtls.RoleBackendAPI, false},
		{"BACKEND-UP", mtls.RoleBackendReg, true},
		{"BACKEND-UP", mtls.RoleIMAPLogin, false},
		{"BACKEND-UNREACHABLE", mtls.RolePOP3Login, true},
		{"BACKEND-UNREACHABLE", mtls.RoleBackendReg, false},
		{"USER-KICK", mtls.RoleJMAPLogin, false},
		{"PING", mtls.RoleBackendReg, true},
		{"QUIT", mtls.RoleIMAPLogin, true},
	} {
		if got := mtls.DirectorCommandAllowed(tc.cmd, tc.role); got != tc.want {
			t.Errorf("%s from %s: %v, want %v", tc.cmd, tc.role, got, tc.want)
		}
	}
}
