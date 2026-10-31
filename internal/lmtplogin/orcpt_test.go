package lmtplogin

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/v2/internal/loginproto"
)

// fakeBackend reads the preamble, speaks enough LMTP for one recipient, and
// reports the RCPT line it got.
func fakeBackend(t *testing.T) (string, <-chan string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	rcpt := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		rd := bufio.NewReader(c)
		rd.ReadString('\n') // preamble
		fmt.Fprint(c, "220 backend\r\n")
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.Fields(line)[0]); cmd {
			case "LHLO":
				fmt.Fprint(c, "250-backend\r\n250 PIPELINING\r\n")
			case "RCPT":
				rcpt <- strings.TrimSpace(line)
				fmt.Fprint(c, "250 2.1.5 OK\r\n")
			case "DATA":
				fmt.Fprint(c, "354 go\r\n")
				for {
					l, err := rd.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
				}
				fmt.Fprint(c, "250 2.0.0 <u@example.com> Saved\r\n")
			case "QUIT":
				fmt.Fprint(c, "221 bye\r\n")
				return
			default:
				fmt.Fprint(c, "250 OK\r\n")
			}
		}
	}()
	return l.Addr().String(), rcpt
}

// The relay's ORCPT goes on to the backend, which announces no DSN; without
// one nothing is added (#2280).
func TestORCPTGoesOnToTheBackend(t *testing.T) {
	cases := []struct {
		name  string
		orcpt goSmtp.RcptOptions
		want  string
	}{
		{"given", goSmtp.RcptOptions{OriginalRecipientType: goSmtp.DSNAddressTypeRFC822, OriginalRecipient: "first@example.com"},
			"RCPT TO:<u@example.com> ORCPT=RFC822;first@example.com"},
		{"none", goSmtp.RcptOptions{}, "RCPT TO:<u@example.com>"},
	}
	for _, c := range cases {
		addr, rcpt := fakeBackend(t)
		pre := loginproto.Preamble{Addr: "192.0.2.1", User: "u@example.com", Token: "t"}
		if err := fanOutOne(addr, "login.test", "a@ext.test", "u@example.com", c.orcpt, []byte("Subject: x\r\n\r\nb\r\n"), pre, 5*time.Second, nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := <-rcpt; got != c.want {
			t.Errorf("%s: backend got %q, want %q", c.name, got, c.want)
		}
	}
}

// lmtp-login takes ORCPT from the relay without offering DSN.
func TestLMTPLoginTakesORCPT(t *testing.T) {
	s := New(testOpts(Options{Hostname: "lmtp.test"}))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.srv.Serve(l) //nolint:errcheck
	defer s.srv.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rd := bufio.NewReader(c)
	rd.ReadString('\n')
	fmt.Fprint(c, "LHLO relay\r\n")
	var caps []string
	for {
		line, _ := rd.ReadString('\n')
		caps = append(caps, line)
		if strings.HasPrefix(line, "250 ") || line == "" {
			break
		}
	}
	if strings.Contains(strings.Join(caps, ""), "DSN") {
		t.Errorf("DSN offered: %v", caps)
	}
	fmt.Fprint(c, "MAIL FROM:<a@ext.test>\r\n")
	rd.ReadString('\n')
	fmt.Fprint(c, "RCPT TO:<u@example.com> ORCPT=rfc822;first@example.com\r\n")
	if line, _ := rd.ReadString('\n'); strings.HasPrefix(line, "504") {
		t.Errorf("RCPT with ORCPT refused as unimplemented: %q", line)
	}
}

type statusSink map[string]error

func (s statusSink) SetStatus(rcpt string, err error) { s[rcpt] = err }

// Through the session: the ORCPT a relay gives with RCPT is the one the
// backend receives for that recipient.
func TestORCPTThroughTheSession(t *testing.T) {
	addr, rcpt := fakeBackend(t)
	s := &session{opts: testOpts(Options{AuthMasterAddr: startTestAuth(t), BackendAddr: addr, Hostname: "login.test"}), peerIP: "192.0.2.1"}
	if err := s.Mail("a@ext.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Rcpt("u@example.com", &goSmtp.RcptOptions{OriginalRecipientType: goSmtp.DSNAddressTypeRFC822, OriginalRecipient: "first@example.com"}); err != nil {
		t.Fatal(err)
	}
	status := statusSink{}
	if err := s.LMTPData(strings.NewReader("Subject: x\r\n\r\nb\r\n"), status); err != nil {
		t.Fatal(err)
	}
	if got := <-rcpt; got != "RCPT TO:<u@example.com> ORCPT=RFC822;first@example.com" {
		t.Errorf("backend got %q (status %v)", got, status)
	}
}
