package sieve

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/foxcpp/go-sieve/interp"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// markStore records the vacation marks; nothing else of the store is used.
type markStore struct {
	ScriptStore
	marked []int
}

func (m *markStore) VacationSent(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

func (m *markStore) MarkVacationSent(_ context.Context, _, _, _, _ string, ttl int) error {
	m.marked = append(m.marked, ttl)
	return nil
}

type sentReply struct {
	from string
	to   []string
	msg  string
}

func vacationConfig() config.SieveConfig {
	return config.Defaults().Sieve
}

func TestVacationReply(t *testing.T) {
	const user, sender = "u@example.com", "bob@ext.test"
	base := "From: Bob <bob@ext.test>\r\nTo: u@example.com\r\nSubject: Hello\r\nMessage-ID: <m1@ext.test>\r\n"
	cases := []struct {
		name     string
		cfg      func(*config.SieveConfig)
		orig     string // OrigRcpt
		envFrom  string // "" = sender
		headers  string
		resp     interp.VacationResponse
		sent     bool
		envelope string   // the reply's envelope sender
		has      []string // in the reply
		hasNot   []string
		marked   []int // nil: the default period
		noMark   bool
	}{
		{name: "addressed to the user in To", headers: base, sent: true,
			has: []string{"From: <u@example.com>\r\n", "To: \"Bob\" <bob@ext.test>\r\n"}},
		{name: "not addressed to the user (§4.5)", headers: "From: bob@ext.test\r\nTo: list@ext.test\r\nSubject: Hello\r\n"},
		{name: "the user in Cc", headers: "From: bob@ext.test\r\nTo: x@ext.test\r\nCc: Someone <U@Example.com>\r\nSubject: Hello\r\n", sent: true},
		{name: "the user in Resent-To", headers: "From: bob@ext.test\r\nTo: x@ext.test\r\nResent-To: u@example.com\r\nSubject: Hello\r\n", sent: true},
		{name: ":addresses in To, the reply is from it", headers: "From: bob@ext.test\r\nTo: alias@example.com\r\nSubject: Hello\r\n",
			resp: interp.VacationResponse{Addresses: []string{"alias@example.com"}}, sent: true, has: []string{"From: <alias@example.com>\r\n"}},
		{name: "check_recipient off replies to any message", cfg: func(c *config.SieveConfig) { c.VacationCheckRecipient = false },
			headers: "From: bob@ext.test\r\nTo: list@ext.test\r\nSubject: Hello\r\n", sent: true, has: []string{"From: <u@example.com>\r\n"}},
		{name: "not to the user's own address (§4.6)", envFrom: user, headers: base},
		{name: "not to a list's message: List-Id", headers: base + "List-Id: <x.ext.test>\r\n"},
		{name: "not to a list's message: List-Unsubscribe alone", headers: base + "List-Unsubscribe: <mailto:x@ext.test>\r\n"},
		{name: "not to a list's message: List-Archive alone", headers: base + "List-Archive: <https://ext.test/a>\r\n"},
		{name: "not to automatic mail", headers: base + "Auto-Submitted: auto-generated\r\n"},
		{name: "not to automatic mail in a second Auto-Submitted", headers: base + "Auto-Submitted: no\r\nAuto-Submitted: auto-replied\r\n"},
		{name: "Auto-Submitted: no is answered", headers: base + "Auto-Submitted: No\r\n", sent: true},
		{name: "not to bulk mail", headers: base + "Precedence: Bulk\r\n"},
		{name: "not when X-Auto-Response-Suppress asks: OOF", headers: base + "X-Auto-Response-Suppress: DR, oof\r\n"},
		{name: "not when X-Auto-Response-Suppress asks: All", headers: base + "X-Auto-Response-Suppress: All\r\n"},
		{name: "X-Auto-Response-Suppress without All or OOF is answered", headers: base + "X-Auto-Response-Suppress: DR, RN\r\n", sent: true},
		{name: "not to MAILER-DAEMON", envFrom: "mailer-daemon@ext.test", headers: base},
		{name: "not to LISTSERV", envFrom: "LISTSERV@ext.test", headers: base},
		{name: "not to majordomo", envFrom: "Majordomo@ext.test", headers: base},
		{name: "not to a -request address", envFrom: "news-request@ext.test", headers: base},
		{name: "not to an owner- address", envFrom: "owner-news@ext.test", headers: base},
		{name: "a sender merely containing owner is answered", envFrom: "theowner-x@ext.test", headers: base, sent: true},
		{name: "not to an address of :addresses, whatever its case", envFrom: "Alias@Example.com", headers: base,
			resp: interp.VacationResponse{Addresses: []string{"alias@example.com"}}},
		{name: "the original recipient counts with use_original_recipient", cfg: func(c *config.SieveConfig) { c.VacationUseOriginalRecipient = true },
			orig: "first@example.com", headers: "From: bob@ext.test\r\nTo: first@example.com\r\nSubject: Hello\r\n", sent: true,
			has: []string{"From: <first@example.com>\r\n"}},
		{name: "the original recipient ignored without it", orig: "first@example.com",
			headers: "From: bob@ext.test\r\nTo: first@example.com\r\nSubject: Hello\r\n"},
		{name: "a plain body gets text/plain", headers: base, resp: interp.VacationResponse{Body: "away\n"}, sent: true,
			has: []string{"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\naway\r\n"}},
		{name: ":mime keeps the body's own headers", headers: base,
			resp: interp.VacationResponse{IsMime: true, Body: "Content-Type: text/html\r\n\r\n<p>away</p>\r\n"}, sent: true,
			has: []string{"MIME-Version: 1.0\r\nContent-Type: text/html\r\n\r\n<p>away</p>\r\n"}, hasNot: []string{"text/plain"}},
		{name: "default subject is Auto: and the message's", headers: base, sent: true, has: []string{"Subject: Auto: Hello\r\n"}},
		{name: "an encoded subject is decoded into it", headers: "From: bob@ext.test\r\nTo: u@example.com\r\nSubject: =?utf-8?q?Caf=C3=A9?=\r\n", sent: true,
			has: []string{"Subject: =?utf-8?q?Auto:_Caf=C3=A9?=\r\n"}},
		{name: "the subject template", cfg: func(c *config.SieveConfig) { c.VacationDefaultSubjectTemplate = "Away: %{subject}" },
			headers: base, sent: true, has: []string{"Subject: Away: Hello\r\n"}},
		{name: "no subject: Automated reply", headers: "From: bob@ext.test\r\nTo: u@example.com\r\n", sent: true, has: []string{"Subject: Automated reply\r\n"}},
		{name: "no subject: default_subject", cfg: func(c *config.SieveConfig) { c.VacationDefaultSubject = "I am away" },
			headers: "From: bob@ext.test\r\nTo: u@example.com\r\n", sent: true, has: []string{"Subject: I am away\r\n"}},
		{name: "the script's subject wins", headers: base, resp: interp.VacationResponse{Subject: "Out", SubjectGiven: true}, sent: true,
			has: []string{"Subject: Out\r\n"}},
		{name: "a subject cannot add a header", headers: base, resp: interp.VacationResponse{Subject: "Out\r\nBcc: x@ext.test", SubjectGiven: true}, sent: true,
			has: []string{"Subject: Out Bcc: x@ext.test\r\n"}, hasNot: []string{"\r\nBcc:"}},
		{name: "In-Reply-To and References from the message (§5.8)", headers: base, sent: true,
			has: []string{"In-Reply-To: <m1@ext.test>\r\nReferences: <m1@ext.test>\r\n"}},
		{name: "References extended", headers: base + "References: <m0@ext.test>\r\n", sent: true,
			has: []string{"In-Reply-To: <m1@ext.test>\r\nReferences: <m0@ext.test> <m1@ext.test>\r\n"}},
		{name: "a period under the minimum is raised", headers: base, resp: interp.VacationResponse{PeriodGiven: true, PeriodSeconds: 60}, sent: true, marked: []int{86400}},
		{name: "a period over the maximum is lowered", headers: base, resp: interp.VacationResponse{PeriodGiven: true, PeriodSeconds: 90 * 86400}, sent: true, marked: []int{5184000}},
		{name: "the default period", cfg: func(c *config.SieveConfig) { c.VacationDefaultPeriod = 2 * 86400 }, headers: base, sent: true, marked: []int{2 * 86400}},
		{name: "a zero period marks nothing", cfg: func(c *config.SieveConfig) { c.VacationMinPeriod = 0 },
			headers: base, resp: interp.VacationResponse{PeriodGiven: true}, sent: true, noMark: true},
		{name: "send_from_recipient", cfg: func(c *config.SieveConfig) { c.VacationSendFromRecipient = true }, headers: base, sent: true, envelope: user},
		{name: "to_header_ignore_envelope", cfg: func(c *config.SieveConfig) { c.VacationToHeaderIgnoreEnvelope = true },
			envFrom: "bounces@ext.test", headers: base, sent: true, has: []string{"To: \"Bob\" <bob@ext.test>\r\n"}},
		{name: "the envelope sender when no header names it", envFrom: "bounces@ext.test", headers: base, sent: true,
			has: []string{"To: <bounces@ext.test>\r\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := vacationConfig()
			if c.cfg != nil {
				c.cfg(&cfg)
			}
			var got []sentReply
			s := &Sender{cfg: cfg, send: func(_ context.Context, from string, to []string, body *bytes.Reader) error {
				b, _ := io.ReadAll(body)
				got = append(got, sentReply{from, to, string(b)})
				return nil
			}}
			store := &markStore{}
			from := c.envFrom
			if from == "" {
				from = sender
			}
			raw := c.headers + "\r\nbody\r\n"
			opts := FilterOptions{Username: user, EnvFrom: from, EnvTo: user, OrigRcpt: c.orig, MsgRaw: []byte(raw)}
			if err := s.sendVacation(context.Background(), store, opts, parseHeaders([]byte(raw)), c.resp); err != nil {
				t.Fatal(err)
			}
			if !c.sent {
				if len(got) != 0 {
					t.Fatalf("a reply was sent: %q", got[0].msg)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("%d replies sent, want 1", len(got))
			}
			r := got[0]
			if r.from != c.envelope || len(r.to) != 1 || r.to[0] != from {
				t.Errorf("envelope %q -> %v, want %q -> [%s]", r.from, r.to, c.envelope, from)
			}
			for _, h := range c.has {
				if !strings.Contains(r.msg, h) {
					t.Errorf("the reply lacks %q:\n%s", h, r.msg)
				}
			}
			for _, h := range c.hasNot {
				if strings.Contains(r.msg, h) {
					t.Errorf("the reply carries %q:\n%s", h, r.msg)
				}
			}
			want := c.marked
			switch {
			case c.noMark:
				want = nil
			case want == nil:
				want = []int{cfg.VacationDefaultPeriod}
			}
			if fmt.Sprint(store.marked) != fmt.Sprint(want) {
				t.Errorf("marked %v, want %v", store.marked, want)
			}
		})
	}
}
