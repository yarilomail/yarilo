package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/yarilomail/yarilo/pkg/jmapcore"

	"github.com/coder/websocket"
)

// The listen mode stands in for a JMAP client holding push open on one
// backend, for hack/stand/push-latency.sh; it runs no checks.
var (
	flagPushListen     = flag.String("push-listen", "", "listen mode: a backend's JMAP WebSocket URL, e.g. wss://10.1.2.3:10443/jmap/ws/; prints one line per StateChange naming Email")
	flagPushListenUser = flag.String("push-listen-user", "", "account the listen mode subscribes as, sent as the login hop's forwarded identity")
	flagPushListenTLS  = flag.String("push-listen-tls-dir", "/etc/yarilo/internal-tls", "directory with the login hop's tls.crt, tls.key and ca.crt")
)

func runPushListen() error {
	dir := *flagPushListenTLS
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		return err
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return errors.New("no CA in ca.crt")
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: *flagInternalServerName, MinVersion: tls.VersionTLS12}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h := http.Header{}
	h.Set("X-Yarilo-User", *flagPushListenUser)
	h.Set("X-Session-ID", "pushlisten000000")
	h.Set("X-Proxy-TTL", "4")
	ws, _, err := websocket.Dial(ctx, *flagPushListen, &websocket.DialOptions{ //nolint:bodyclose // the library owns the handshake response and nils its body
		HTTPClient:   &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}},
		HTTPHeader:   h,
		Subprotocols: []string{"jmap"},
	})
	if err != nil {
		return err
	}
	defer ws.CloseNow() //nolint:errcheck
	for _, msg := range []string{
		`{"@type":"WebSocketPushEnable","dataTypes":["Email"]}`,
		`{"@type":"Request","id":"e","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c0"]]}`,
	} {
		if err := ws.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			return err
		}
	}
	if _, err := wsNextOf(ctx, ws, "Response"); err != nil {
		return err
	}
	fmt.Println("ready")
	for {
		data, err := wsNextOf(ctx, ws, "StateChange")
		if err != nil {
			return err
		}
		var change struct {
			Changed map[string]map[string]string `json:"changed"`
		}
		// The hop has no session to read the id from; it is the server's encoding.
		if json.Unmarshal(data, &change) == nil && change.Changed[jmapcore.AccountID(*flagPushListenUser)]["Email"] != "" {
			fmt.Println("state Email")
		}
	}
}
