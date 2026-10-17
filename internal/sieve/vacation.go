package sieve

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"mime"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/foxcpp/go-sieve/interp"
)

// myAddressHeaders are where the user's address must appear for a reply (RFC 5230 §4.5).
var myAddressHeaders = []string{"To", "Cc", "Bcc", "Resent-To", "Resent-Cc", "Resent-Bcc"}

// listHeaders mark a list's message; vacation does not answer one (RFC 5230 §4.5).
var listHeaders = []string{"List-Id", "List-Owner", "List-Subscribe", "List-Post", "List-Unsubscribe", "List-Help", "List-Archive"}

// senderHeaders give the reply's To, with the sender's display name.
var senderHeaders = []string{"Sender", "Resent-From", "From"}

// sendVacation sends an RFC 5230 auto-reply to the envelope sender, unless the
// message is the user's own, a list's or automatic, or not addressed to the user.
func (s *Sender) sendVacation(
	ctx context.Context,
	store ScriptStore,
	opts FilterOptions,
	hdr textproto.MIMEHeader,
	resp interp.VacationResponse,
) error {
	sender, recipient := opts.EnvFrom, opts.EnvTo
	if sender == "" || sender == "<>" || recipient == "" {
		return nil
	}
	// §4.6: never to the user, by the envelope or by the script's :addresses.
	if strings.EqualFold(sender, recipient) || containsFold(resp.Addresses, sender) {
		return nil
	}
	if isAutomaticMail(hdr) || isSystemAddress(sender) {
		return nil
	}
	smtpFrom, replyFrom, ok := s.vacationAddressedTo(opts, hdr, resp.Addresses)
	if !ok {
		slog.Debug("sieve/sender: vacation skipped, the user is not a recipient in the headers", "user", opts.Username)
		return nil
	}

	sent, err := store.VacationSent(ctx, opts.Username, opts.HomeDir, resp.Handle, sender)
	if err != nil {
		return fmt.Errorf("sieve/sender: vacation dedup lookup: %w", err)
	}
	if sent {
		return nil
	}

	from := oneLine(resp.From)
	if from == "" {
		from = (&mail.Address{Address: replyFrom}).String()
	}
	subject := oneLine(resp.Subject)
	if !resp.SubjectGiven {
		subject = s.defaultSubject(hdr)
	}
	msg := buildVacationReply(from, s.replyTo(hdr, sender), subject, resp.Body, resp.IsMime, hdr)

	envFrom := ""
	if s.cfg.VacationSendFromRecipient {
		envFrom = smtpFrom
	}
	send := s.send
	if send == nil {
		send = s.submit
	}
	if err := send(ctx, envFrom, []string{sender}, bytes.NewReader(msg)); err != nil {
		return fmt.Errorf("sieve/sender: vacation send: %w", err)
	}

	if period := s.vacationPeriod(resp); period > 0 {
		if err := store.MarkVacationSent(ctx, opts.Username, opts.HomeDir, resp.Handle, sender, period); err != nil {
			slog.Warn("sieve/sender: vacation dedup mark failed", "user", opts.Username, "err", err)
		}
	}
	return nil
}

// isAutomaticMail is a list's message, an automatic one (Auto-Submitted other
// than no), bulk by Precedence, or one asking for no auto-reply.
func isAutomaticMail(hdr textproto.MIMEHeader) bool {
	for _, name := range listHeaders {
		if len(hdr.Values(name)) > 0 {
			return true
		}
	}
	for _, v := range hdr.Values("Auto-Submitted") {
		if !strings.EqualFold(strings.TrimSpace(v), "no") {
			return true
		}
	}
	for _, v := range hdr.Values("Precedence") {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "junk", "bulk", "list":
			return true
		}
	}
	for _, v := range hdr.Values("X-Auto-Response-Suppress") {
		for _, flag := range strings.Split(v, ",") {
			if f := strings.TrimSpace(flag); strings.EqualFold(f, "All") || strings.EqualFold(f, "OOF") {
				return true
			}
		}
	}
	return false
}

// isSystemAddress is a sender a person does not read: a daemon or a list's
// request and owner addresses, judged as the reference does.
func isSystemAddress(addr string) bool {
	local := addr
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		local = addr[:i]
	}
	return strings.EqualFold(local, "MAILER-DAEMON") || strings.EqualFold(local, "LISTSERV") ||
		strings.EqualFold(local, "majordomo") || strings.Contains(local, "-request") || strings.HasPrefix(local, "owner-")
}

// vacationAddressedTo finds the user (envelope recipient, original recipient,
// :addresses) in the recipient headers; it returns the reply's sender and From.
func (s *Sender) vacationAddressedTo(opts FilterOptions, hdr textproto.MIMEHeader, addresses []string) (smtpFrom, replyFrom string, ok bool) {
	orig := ""
	if s.cfg.VacationUseOriginalRecipient {
		orig = opts.OrigRcpt
	}
	own := opts.EnvTo
	if orig != "" {
		own = orig
	}
	for _, name := range myAddressHeaders {
		listed := headerAddresses(hdr[textproto.CanonicalMIMEHeaderKey(name)])
		if len(listed) == 0 {
			continue
		}
		switch {
		case containsFold(listed, opts.EnvTo):
			return opts.EnvTo, opts.EnvTo, true
		case orig != "" && containsFold(listed, orig):
			return orig, orig, true
		}
		for _, a := range addresses {
			if containsFold(listed, a) {
				return own, a, true
			}
		}
	}
	if s.cfg.VacationCheckRecipient {
		return "", "", false
	}
	return own, opts.EnvTo, true
}

// replyTo is the first sender-header address that is the envelope sender (any
// address with to_header_ignore_envelope), with its name; else the envelope sender.
func (s *Sender) replyTo(hdr textproto.MIMEHeader, sender string) string {
	for _, name := range senderHeaders {
		v := hdr.Get(name)
		if v == "" {
			continue
		}
		list, err := mail.ParseAddressList(v)
		if err != nil {
			continue
		}
		for _, a := range list {
			if s.cfg.VacationToHeaderIgnoreEnvelope || strings.EqualFold(a.Address, sender) {
				return a.String()
			}
		}
	}
	return (&mail.Address{Address: sender}).String()
}

// defaultSubject is "Auto: <subject>" or the template with %{subject}; a
// message with no subject gets sieve_vacation_default_subject.
func (s *Sender) defaultSubject(hdr textproto.MIMEHeader) string {
	vals, present := hdr["Subject"]
	if !present || len(vals) == 0 {
		if s.cfg.VacationDefaultSubject != "" {
			return oneLine(s.cfg.VacationDefaultSubject)
		}
		return "Automated reply"
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(vals[0])
	if err != nil {
		subject = vals[0]
	}
	if s.cfg.VacationDefaultSubjectTemplate == "" {
		return oneLine("Auto: " + subject)
	}
	return oneLine(strings.ReplaceAll(s.cfg.VacationDefaultSubjectTemplate, "%{subject}", subject))
}

// vacationPeriod is the script's period, or the default, bounded by the
// minimum and the maximum (0 = no maximum).
func (s *Sender) vacationPeriod(resp interp.VacationResponse) int {
	period := s.cfg.VacationDefaultPeriod
	if resp.PeriodGiven {
		period = resp.PeriodSeconds
	}
	if period < s.cfg.VacationMinPeriod {
		return s.cfg.VacationMinPeriod
	}
	if s.cfg.VacationMaxPeriod > 0 && period > s.cfg.VacationMaxPeriod {
		return s.cfg.VacationMaxPeriod
	}
	return period
}

// buildVacationReply writes the reply. A :mime body carries its own MIME
// headers, so only a plain one gets Content-Type.
func buildVacationReply(from, to, subject, body string, isMime bool, orig textproto.MIMEHeader) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Message-ID: <%d.sieve-vacation@yarilo>\r\n", time.Now().UnixNano())
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 +0000"))
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	// §5.8: the reply joins the thread of the message it answers.
	msgID := strings.TrimSpace(orig.Get("Message-Id"))
	refs := oneLine(strings.TrimSpace(orig.Get("References")))
	switch {
	case msgID != "" && refs != "":
		fmt.Fprintf(&b, "In-Reply-To: %s\r\nReferences: %s %s\r\n", oneLine(msgID), refs, oneLine(msgID))
	case msgID != "":
		fmt.Fprintf(&b, "In-Reply-To: %s\r\nReferences: %s\r\n", oneLine(msgID), oneLine(msgID))
	case refs != "":
		fmt.Fprintf(&b, "References: %s\r\n", refs)
	}
	b.WriteString("Auto-Submitted: auto-replied\r\n")
	b.WriteString("X-Auto-Response-Suppress: OOF, AutoReply\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	if !isMime {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
		b.WriteString("\r\n")
	}
	b.WriteString(crlf(body))
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

// headerAddresses parses every value of one header; a value that does not
// parse as a list is read address by address.
func headerAddresses(vals []string) []string {
	var out []string
	for _, v := range vals {
		if list, err := mail.ParseAddressList(v); err == nil {
			for _, a := range list {
				out = append(out, a.Address)
			}
			continue
		}
		for _, part := range strings.Split(v, ",") {
			if a, err := mail.ParseAddress(strings.TrimSpace(part)); err == nil {
				out = append(out, a.Address)
			}
		}
	}
	return out
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// oneLine keeps a header value on one line: a script's string must not add headers.
func oneLine(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s))
}

func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}
