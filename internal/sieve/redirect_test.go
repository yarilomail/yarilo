package sieve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

type forwarded struct {
	to  string
	raw string
}

// redirectEngine records what each redirect sends; fail makes sends fail.
func redirectEngine(t *testing.T, mutate func(*config.SieveConfig)) (*Engine, *[]forwarded, *bool) {
	t.Helper()
	cfg := config.Defaults().Sieve
	cfg.Enabled = true
	cfg.SubmissionHost = "submit.invalid:587"
	if mutate != nil {
		mutate(&cfg)
	}
	e := New(cfg, nil, nil, nil)
	var out []forwarded
	fail := false
	e.sender.send = func(_ context.Context, _ string, to []string, body *bytes.Reader) error {
		if fail {
			return errors.New("relay down")
		}
		b, _ := io.ReadAll(body)
		out = append(out, forwarded{to[0], string(b)})
		return nil
	}
	return e, &out, &fail
}

const parityMsg = "Return-Path: <sender@example.com>\r\n" +
	"Received: from a by b;\r\n\tMon, 2 Jan 2006 15:04:05 -0700\r\n" +
	"From: sender@example.com\r\n" +
	"To: u1@example.com\r\n" +
	"Message-ID: <m1@example.com>\r\n" +
	"Subject: Test\r\n\r\nReturn-Path: in the body stays\r\nHello.\r\n"

func parityOpts(user, home, msg string) FilterOptions {
	o := baseOpts(user, home)
	o.MsgRaw = []byte(msg)
	return o
}

// The forwarded message: X-Sieve and X-Sieve-Redirected-From with the
// delivery's recipient first, Return-Path gone, every other byte as it came,
// header continuations and the body included.
func TestRedirectTraceHeadersAndBody(t *testing.T) {
	e, out, _ := redirectEngine(t, nil)
	home := withScript(t, "u1", `redirect "dest@x.test";`)
	if _, err := e.Filter(context.Background(), parityOpts("u1", home, parityMsg)); err != nil {
		t.Fatal(err)
	}
	if len(*out) != 1 {
		t.Fatalf("%d forwarded, want 1", len(*out))
	}
	want := "X-Sieve: yarilo\r\nX-Sieve-Redirected-From: u1@example.com\r\n" +
		strings.Replace(parityMsg, "Return-Path: <sender@example.com>\r\n", "", 1)
	if got := (*out)[0].raw; got != want {
		t.Fatalf("forwarded\n%q\nwant\n%q", got, want)
	}
}

// IMAPSieve has no envelope: the trace header names the user.
func TestRedirectTraceHeaderWithoutEnvelope(t *testing.T) {
	e, out, _ := redirectEngine(t, nil)
	opts := FilterOptions{Username: "u1@example.com", EnvTo: "u1@example.com", MsgRaw: []byte(parityMsg)}
	if !e.redirectOne(context.Background(), opts, parseHeaders(opts.MsgRaw), "dest@x.test") {
		t.Fatal("not sent")
	}
	if !strings.Contains((*out)[0].raw, "\r\nX-Sieve-Redirected-From: u1@example.com\r\n") {
		t.Fatalf("no trace header naming the user:\n%s", (*out)[0].raw)
	}
}

// A Message-ID is added only when the message has none.
func TestRedirectAddsMessageIDOnlyWhenMissing(t *testing.T) {
	e, out, _ := redirectEngine(t, nil)
	home := withScript(t, "u1", `redirect "dest@x.test";`)
	noID := strings.Replace(parityMsg, "Message-ID: <m1@example.com>\r\n", "", 1)
	for _, m := range []string{parityMsg, noID} {
		if _, err := e.Filter(context.Background(), parityOpts("u1", home, m)); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(strings.ToLower((*out)[0].raw), "message-id:"); n != 1 {
		t.Fatalf("with an id: %d Message-ID fields, want 1", n)
	}
	added := strings.Split((*out)[1].raw, "\r\n")[2]
	if !strings.HasPrefix(added, "Message-ID: <") || !strings.HasSuffix(added, "@localhost>") {
		t.Fatalf("without an id: third line %q, want an added Message-ID", added)
	}
}

// Two mailboxes forwarding to each other: the message stops when it comes back
// to a mailbox it already left, and stays there.
func TestRedirectLoopStops(t *testing.T) {
	// No duplicate records, so the trace header alone has to stop it.
	e, out, _ := redirectEngine(t, func(c *config.SieveConfig) { c.RedirectDuplicatePeriod = 0 })
	h1 := withScript(t, "u1", `redirect "u2@example.com";`)
	h2 := withScript(t, "u2", `redirect "u1@example.com";`)
	msg := parityMsg
	for hop, step := range []struct{ user, home string }{{"u1", h1}, {"u2", h2}, {"u1", h1}} {
		res, err := e.Filter(context.Background(), parityOpts(step.user, step.home, msg))
		if err != nil {
			t.Fatal(err)
		}
		if hop < 2 {
			if len(*out) != hop+1 {
				t.Fatalf("hop %d: %d forwarded, want %d", hop, len(*out), hop+1)
			}
			msg = (*out)[hop].raw
			continue
		}
		if len(*out) != 2 {
			t.Fatalf("the loop went on: %d forwarded, want 2", len(*out))
		}
		if !inInbox(res) {
			t.Fatal("the looped message was kept nowhere")
		}
	}
}

// The same message to the same address is forwarded once within the period,
// and again after it; the skipped one stays in INBOX and takes no slot of the
// send limit.
func TestRedirectDuplicate(t *testing.T) {
	e, out, _ := redirectEngine(t, func(c *config.SieveConfig) {
		c.RedirectDuplicatePeriod = 1
		c.SendRateLimit = 2
	})
	home := withScript(t, "u1", `redirect "dest@x.test";`)
	res := make([]*FilterResult, 0, 2)
	for range 2 {
		r, err := e.Filter(context.Background(), parityOpts("u1", home, parityMsg))
		if err != nil {
			t.Fatal(err)
		}
		res = append(res, r)
	}
	if len(*out) != 1 || !inInbox(res[1]) {
		t.Fatalf("%d forwarded, second in INBOX %v; want 1 and true", len(*out), inInbox(res[1]))
	}
	time.Sleep(2100 * time.Millisecond)
	if _, err := e.Filter(context.Background(), parityOpts("u1", home, parityMsg)); err != nil {
		t.Fatal(err)
	}
	if len(*out) != 2 {
		t.Fatalf("after the period: %d forwarded, want 2 (the duplicate took no slot of the limit of 2)", len(*out))
	}
}

// A send that failed records nothing: the next delivery forwards it.
func TestRedirectFailedSendRecordsNoDuplicate(t *testing.T) {
	e, out, fail := redirectEngine(t, nil)
	home := withScript(t, "u1", `redirect "dest@x.test";`)
	*fail = true
	if _, err := e.Filter(context.Background(), parityOpts("u1", home, parityMsg)); err != nil {
		t.Fatal(err)
	}
	*fail = false
	if _, err := e.Filter(context.Background(), parityOpts("u1", home, parityMsg)); err != nil {
		t.Fatal(err)
	}
	if len(*out) != 1 {
		t.Fatalf("%d forwarded after a failed send, want 1", len(*out))
	}
}

// dropHeader leaves a body line that looks like the field alone.
func TestDropHeaderStopsAtTheBody(t *testing.T) {
	in := "A: 1\r\nReturn-Path: <x>\r\n\tcont\r\nB: 2\r\n\r\nReturn-Path: body\r\n"
	if got := string(dropHeader([]byte(in), "Return-Path")); got != "A: 1\r\nB: 2\r\n\r\nReturn-Path: body\r\n" {
		t.Fatalf("%q", got)
	}
}
