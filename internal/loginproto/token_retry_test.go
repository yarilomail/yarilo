package loginproto

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	authclient "github.com/yarilomail/yarilo/v2/internal/auth/client"
	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/pkg/authtoken"
)

// A backend that died before its greeting leaves the token to the retry, and
// the session that opens retires it, as the stand's retry needed (#2259).
func TestATokenOpensOneSessionAndSurvivesABackendThatDied(t *testing.T) {
	store := authtoken.New(time.Minute)
	t.Cleanup(store.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	authLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go protocol.NewServer(nil, protocol.WithTokenStore(store)).Serve(ctx, authLn) //nolint:errcheck

	tok, err := store.Issue("u52@d00002.test", "sess-1", "imap")
	if err != nil {
		t.Fatal(err)
	}
	pre := Preamble{SessionID: "sess-1", User: "u52@d00002.test", Token: tok}.Format()

	// session opens one backend's session with the token and returns it.
	session := func(t *testing.T) *PreambleConn {
		t.Helper()
		base, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		l := &PreambleListener{Listener: base, AuthAddr: authLn.Addr().String(), ExpectedService: "imap"}
		t.Cleanup(func() { l.Close() })
		c, err := net.Dial("tcp", base.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		if _, err := c.Write([]byte(pre)); err != nil {
			t.Fatal(err)
		}
		got := make(chan net.Conn, 1)
		go func() {
			if pc, err := l.Accept(); err == nil {
				got <- pc
			}
		}()
		select {
		case pc := <-got:
			return pc.(*PreambleConn)
		case <-time.After(5 * time.Second):
			return nil
		}
	}

	dying := session(t)
	if dying == nil {
		t.Fatal("the first backend did not open the session")
	}
	dying.Close() // gone before its greeting

	retry := session(t)
	if retry == nil {
		t.Fatal("the retry on a second backend was refused: the dead backend's check took the token")
	}
	if _, err := retry.Write([]byte("* OK ready\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, _, ok := store.Validate(tok); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session opened and the token still opens another")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A second retirement is the trace of a token that opened another session:
	// it reaches the backend as an error, which the backend logs.
	cl, err := authclient.Dial(authLn.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if err := cl.Consume(tok); !errors.Is(err, authclient.ErrTokenGone) {
		t.Errorf("retiring a retired token answered %v, want ErrTokenGone", err)
	}
}
