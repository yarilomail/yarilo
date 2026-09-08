package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// pop3Session is a logged-in maildrop, closed by the caller.
type pop3Session struct {
	*pop3Client
	conn net.Conn
}

func (s *pop3Session) close() {
	_ = s.ok("QUIT")
	_ = s.conn.Close()
}

// quit ends the session the way RFC 1939 makes deletions final: only the
// UPDATE state a QUIT enters removes what DELE marked.
func (s *pop3Session) quit() error {
	err := s.ok("QUIT")
	_ = s.conn.Close()
	return err
}

// pop3Endpoint is where POP3 answers and how it is secured.
func pop3Endpoint() (endpoint, error) {
	mode, err := parseTLSMode("pop3-tls", *flagPOP3TLS)
	if err != nil {
		return endpoint{}, err
	}
	port := *flagPOP3Port
	if port == "" {
		port = *flagPOP3SPort
	}
	return endpoint{
		name: "pop3", host: pop3Host(), port: port, mode: mode,
		upgrade: lineUpgrade("STLS", "+OK"),
	}, nil
}

func pop3Login(user, pass string) (*pop3Session, error) {
	ep, err := pop3Endpoint()
	if err != nil {
		return nil, err
	}
	conn, r, err := ep.dial()
	if err != nil {
		return nil, err
	}
	p := &pop3Session{pop3Client: &pop3Client{conn: conn, r: r}, conn: conn}
	// STARTTLS read the greeting to send STLS into a settled stream; the
	// upgraded session opens on the command that follows, with none of its own.
	if ep.mode != tlsSTARTTLS {
		if _, err := p.line(); err != nil {
			conn.Close() //nolint:errcheck
			return nil, fmt.Errorf("greeting: %w", err)
		}
	}
	if err := p.ok("USER " + user); err != nil {
		conn.Close() //nolint:errcheck
		return nil, err
	}
	if err := p.ok("PASS " + pass); err != nil {
		conn.Close() //nolint:errcheck
		return nil, err
	}
	return p, nil
}

// stat returns the two numbers STAT reports: how many messages the maildrop
// holds and how many octets they are together.
func (p *pop3Client) stat() (count int, octets int64, err error) {
	fmt.Fprintf(p.conn, "STAT\r\n")
	l, err := p.line()
	if err != nil {
		return 0, 0, fmt.Errorf("STAT: %w", err)
	}
	if !strings.HasPrefix(l, "+OK") {
		return 0, 0, fmt.Errorf("STAT refused: %s", l)
	}
	if _, err := fmt.Sscanf(l, "+OK %d %d", &count, &octets); err != nil {
		return 0, 0, fmt.Errorf("STAT answered %q, which is not \"+OK <count> <octets>\"", l)
	}
	return count, octets, nil
}

// uidl reads the whole listing: RFC 1939 gives each message an id unique within
// the maildrop and persistent across sessions.
func (p *pop3Client) uidl() (map[int]string, error) {
	if err := p.ok("UIDL"); err != nil {
		return nil, err
	}
	out := map[int]string{}
	for {
		l, err := p.line()
		if err != nil {
			return nil, fmt.Errorf("UIDL: %w", err)
		}
		if l == "." {
			return out, nil
		}
		num, id, ok := strings.Cut(l, " ")
		if !ok {
			return nil, fmt.Errorf("UIDL row %q carries no id", l)
		}
		n, cerr := strconv.Atoi(num)
		if cerr != nil {
			return nil, fmt.Errorf("UIDL row %q does not start with a message number", l)
		}
		out[n] = id
	}
}

// retr reads one message whole, undoing the byte-stuffing the dot-terminated
// body applies. The count it returns is what a client stores.
func (p *pop3Client) retr(num int) (string, error) {
	if err := p.ok(fmt.Sprintf("RETR %d", num)); err != nil {
		return "", err
	}
	var b strings.Builder
	for {
		l, err := p.line()
		if err != nil {
			return "", fmt.Errorf("RETR: %w", err)
		}
		if l == "." {
			return b.String(), nil
		}
		b.WriteString(strings.TrimPrefix(l, "."))
		b.WriteString("\r\n")
	}
}

// findProbe is the message number the marker is in, newest end first.
func (p *pop3Client) findProbe(marker string) (int, int64, error) {
	sizes, err := p.list()
	if err != nil {
		return 0, 0, err
	}
	for i := len(sizes) - 1; i >= 0; i-- {
		headers, terr := p.top(sizes[i].num)
		if terr != nil {
			return 0, 0, terr
		}
		if strings.Contains(headerValue(headers, "Subject"), marker) {
			return sizes[i].num, sizes[i].octets, nil
		}
	}
	return 0, 0, fmt.Errorf("the probe (marker %s) is not in the maildrop", marker)
}

// deliverPOP3Probe puts one message in the maildrop and waits for it to arrive.
func deliverPOP3Probe(user, pass, marker string) (*pop3Session, int, int64, error) {
	subject := "yarilo smoke pop3 " + marker
	if err := lmtpSend(uniqueID(), "pop3@test.invalid", user, subject, pop3ProbeBody); err != nil {
		return nil, 0, 0, fmt.Errorf("deliver: %w", err)
	}
	deadline := time.Now().Add(*flagTimeout)
	for {
		s, err := pop3Login(user, pass)
		if err != nil {
			return nil, 0, 0, err
		}
		num, octets, ferr := s.findProbe(marker)
		if ferr == nil {
			return s, num, octets, nil
		}
		s.close()
		if time.Now().After(deadline) {
			return nil, 0, 0, ferr
		}
		time.Sleep(200 * time.Millisecond)
	}
}

const pop3ProbeBody = "the pop3 probe body\r\n"

// checkPOP3Cycle walks the maildrop the way a client does: until this row the
// area held one greeting and asserted no octet count (#1734).
func checkPOP3Cycle(user, pass string) error {
	marker := consistencyMarker("pop3")
	s, num, listed, err := deliverPOP3Probe(user, pass, marker)
	if err != nil {
		return err
	}
	defer s.close()
	return verifyMaildrop(s, user, pass, marker, num, listed)
}

// verifyMaildrop is the judgement, apart from the delivery: what STAT, LIST,
// RETR, UIDL and DELE must agree on for a client to have the message.
func verifyMaildrop(s *pop3Session, user, pass, marker string, num int, listed int64) error {
	count, total, err := s.stat()
	if err != nil {
		return err
	}
	sizes, err := s.list()
	if err != nil {
		return err
	}
	if count != len(sizes) {
		return fmt.Errorf("STAT counts %d messages, LIST names %d", count, len(sizes))
	}
	var sum int64
	for _, e := range sizes {
		sum += e.octets
	}
	if total != sum {
		return fmt.Errorf("STAT reports %d octets, the LIST rows add up to %d", total, sum)
	}

	body, err := s.retr(num)
	if err != nil {
		return err
	}
	if !strings.Contains(body, marker) {
		return fmt.Errorf("RETR %d returned a message without the marker", num)
	}
	if int64(len(body)) != listed {
		return fmt.Errorf("LIST promised %d octets for message %d, RETR delivered %d",
			listed, num, len(body))
	}

	ids, err := s.uidl()
	if err != nil {
		return err
	}
	id, named := ids[num]
	if !named || id == "" {
		return fmt.Errorf("UIDL names no id for message %d", num)
	}
	// Uniqueness before the count: a shared id is the more specific fault, and
	// a maildrop that hands one out also miscounts.
	seen := map[string]int{}
	for n, v := range ids {
		if first, dup := seen[v]; dup {
			return fmt.Errorf("messages %d and %d share the uidl %q", first, n, v)
		}
		seen[v] = n
	}
	if len(ids) != count {
		return fmt.Errorf("UIDL lists %d ids for %d messages", len(ids), count)
	}
	return pop3CheckDeletion(user, pass, marker, id)
}

// pop3CheckDeletion walks what RFC 1939 makes a deletion: undone by a session
// that ends without QUIT, final only after one that does.
func pop3CheckDeletion(user, pass, marker, id string) error {
	abandoned, err := pop3Login(user, pass)
	if err != nil {
		return err
	}
	num, _, err := abandoned.findProbe(marker)
	if err != nil {
		abandoned.close()
		return err
	}
	if derr := abandoned.ok(fmt.Sprintf("DELE %d", num)); derr != nil {
		abandoned.close()
		return derr
	}
	// Dropped without QUIT: the UPDATE state is never entered, so nothing the
	// session marked may be gone.
	_ = abandoned.conn.Close()

	after, err := pop3Login(user, pass)
	if err != nil {
		return err
	}
	ids, err := after.uidl()
	if err != nil {
		after.close()
		return err
	}
	if !slicesContainsID(ids, id) {
		after.close()
		return fmt.Errorf("the message is gone after a session that never quit; a DELE it did not commit removed it")
	}
	num, _, err = after.findProbe(marker)
	if err != nil {
		after.close()
		return err
	}
	if derr := after.ok(fmt.Sprintf("DELE %d", num)); derr != nil {
		after.close()
		return derr
	}
	if qerr := after.quit(); qerr != nil {
		return qerr
	}

	final, err := pop3Login(user, pass)
	if err != nil {
		return err
	}
	defer final.close()
	ids, err = final.uidl()
	if err != nil {
		return err
	}
	if slicesContainsID(ids, id) {
		return fmt.Errorf("the message is still in the maildrop after a DELE the session committed with QUIT")
	}
	return nil
}

func slicesContainsID(ids map[int]string, want string) bool {
	for _, v := range ids {
		if v == want {
			return true
		}
	}
	return false
}
