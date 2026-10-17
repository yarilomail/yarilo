package main

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// smtpReply reads one whole reply, every continuation line included: a reader
// that stops at the first line leaves the rest as the next command's reply
// and passes while the session is a reply behind (#2271).
func smtpReply(conn net.Conn) (string, error) {
	for {
		line, err := readLine(conn)
		if err != nil {
			return "", err
		}
		if len(line) < 4 || line[3] != '-' {
			return line, nil
		}
	}
}

// checkSMTPSubmissionSend sends one message through submission and the relay
// behind it, and finds it in the sender's INBOX: what a client does.
func checkSMTPSubmissionSend() error {
	user, pass := *flagIMAPUser, *flagIMAPPass
	conn, _, err := submissionTLS()
	if err != nil {
		return err
	}
	defer conn.Close()
	marker := "xsubmit-" + uniqueID()
	body := fmt.Sprintf("From: <%s>\r\nTo: <%s>\r\nSubject: %s\r\nMessage-ID: <%s@smoketest>\r\n\r\nsent through the relay\r\n.\r\n",
		user, user, marker, marker)
	for _, step := range []struct{ cmd, want string }{
		{plainAuth(user, pass), "235"},
		{"MAIL FROM:<" + user + ">", "250"},
		{"RCPT TO:<" + user + ">", "250"},
		{"DATA", "354"},
		{strings.TrimSuffix(body, "\r\n"), "250"},
	} {
		fmt.Fprintf(conn, "%s\r\n", step.cmd)
		line, err := smtpReply(conn)
		if err != nil {
			return fmt.Errorf("%s: read reply: %w", strings.Fields(step.cmd)[0], err)
		}
		if !strings.HasPrefix(line, step.want) {
			return fmt.Errorf("%s: reply %q, want %s", strings.Fields(step.cmd)[0], line, step.want)
		}
	}
	smtpQuit(conn)
	return findAndRemove(user, pass, marker)
}

// findAndRemove waits for the message in INBOX and expunges it, so no other
// row counts it.
func findAndRemove(user, pass, marker string) error {
	c, err := imapDial()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	deadline := time.Now().Add(*flagTimeout * 3)
	for {
		if _, err := c.selectFolder("INBOX"); err != nil {
			return fmt.Errorf("select INBOX: %w", err)
		}
		uids, err := c.uidSearch("HEADER SUBJECT " + marker)
		if err != nil {
			return fmt.Errorf("uid search: %w", err)
		}
		if len(uids) == 1 {
			return c.deleteUIDs(uids)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the message %s did not reach INBOX through the relay", marker)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
