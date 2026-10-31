package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"

	"github.com/coder/websocket"
)

// The listen mode subscribes as the login hop does and prints a line only for
// the account's Email moving: that line is what push-latency.sh times.
func TestPushListenPrintsTheAccountsEmailChanges(t *testing.T) {
	caPEM, serverCert, clientCertPEM, clientKeyPEM := issueMTLSFixtures(t)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	seen := make(chan http.Header, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"jmap"}})
		if err != nil {
			return
		}
		defer c.CloseNow() //nolint:errcheck
		ctx := r.Context()
		for range 2 { // the enable, then the echo
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
		for _, m := range []string{
			`{"@type":"Response","requestId":"e","methodResponses":[]}`,
			`{"@type":"StateChange","changed":{"other@x":{"Email":"s1"}}}`,
			`{"@type":"StateChange","changed":{"` + jmapcore.AccountID("u@x") + `":{"Mailbox":"s1"}}}`,
			`{"@type":"StateChange","changed":{"` + jmapcore.AccountID("u@x") + `":{"Email":"s2"}}}`,
		} {
			if err := c.Write(ctx, websocket.MessageText, []byte(m)); err != nil {
				return
			}
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	writeFile(t, dir, "tls.crt", clientCertPEM)
	writeFile(t, dir, "tls.key", clientKeyPEM)
	writeFile(t, dir, "ca.crt", caPEM)
	restore := setFlags(map[*string]string{
		flagPushListen:     "wss" + strings.TrimPrefix(srv.URL, "https") + "/jmap/ws/",
		flagPushListenUser: "u@x",
		flagPushListenTLS:  dir,
	})
	defer restore()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan error, 1)
	go func() { done <- runPushListen() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("listen mode did not end when the server closed")
	}
	os.Stdout = stdout
	_ = w.Close()

	var lines []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if got := strings.Join(lines, "|"); got != "ready|state Email" {
		t.Errorf("printed %q, want ready then one Email line", got)
	}
	h := <-seen
	if h.Get("X-Yarilo-User") != "u@x" || h.Get("X-Proxy-TTL") == "" || h.Get("X-Session-ID") == "" {
		t.Errorf("identity headers %v, want the login hop's", h)
	}
}
