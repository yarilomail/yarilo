package imap_test

import (
	"net"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/v2/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/v2/internal/imap"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// ageOpens is mailbox_crypt_opened_total for age bodies, over every driver.
func ageOpens(t *testing.T) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n float64
	for _, mf := range mfs {
		if mf.GetName() != "mailbox_crypt_opened_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "format" && l.GetValue() == "age" {
					n += m.GetCounter().GetValue()
				}
			}
		}
	}
	return n
}

// One FETCH that asks for several sections of an encrypted message opens it
// once: each open pays the key unwrap.
func TestFetchOpensAnEncryptedMessageOncePerResponse(t *testing.T) {
	id, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	layer := mailcrypt.New(mailcrypt.Options{Recipients: []age.Recipient{id.Recipient()}, Identities: []age.Identity{id}})
	srv := imapserver.New(imapserver.Options{
		Mailbox:   maildir.New(maildir.WithCrypt(layer)),
		Index:     file.New(),
		Resolver:  &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"},
		AuthRelay: authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
	})
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	c := dialRaw(t, ln.Addr().String())
	c.login()
	appendEnvelopeMsg(t, c)
	c.cmd(`SELECT INBOX`)
	before := ageOpens(t)
	out := c.cmd(`FETCH 1 (BODY.PEEK[HEADER] BODY.PEEK[TEXT] BODY.PEEK[] BODY.PEEK[]<0.4>)`)
	// Each section reads from its own position: a shared one would start
	// BODY[] and BODY[]<0.4> where the sections before them stopped.
	whole := "BODY[] {" + itoa(len(envelopeMsg)) + "}\r\n" + envelopeMsg
	for _, want := range []string{"BODY[TEXT] {4}\r\nhi\r\n", whole, "BODY[]<0> {4}\r\nFrom", "Message-ID: <m1@example.com>"} {
		// cmd reads line by line and joins with LF.
		if want = strings.ReplaceAll(want, "\r\n", "\n"); !strings.Contains(out, want) {
			t.Fatalf("response has no %q:\n%s", want, out)
		}
	}
	if d := ageOpens(t) - before; d != 1 {
		t.Fatalf("age opens for one FETCH response: %v, want 1", d)
	}
}
