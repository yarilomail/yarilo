package main

import (
	"fmt"
	"strings"
	"time"
)

// fetchEnvelopeProbeSubject marks the message this check appends when it finds
// an empty INBOX, so cleanup removes exactly that one.
const fetchEnvelopeProbeSubject = "yarilo-smoke-envelope-probe"

// fetchRetryDelay is the wait before the same session asks again; a var so
// the tests do not sleep.
var fetchRetryDelay = 2 * time.Second

// checkFetchEnvelope runs the command a mail client issues to draw a message
// list, on an account's REAL INBOX.
//
// The gate missed a crash in exactly this command because everything else
// reaches IMAP through mailboxes the run creates itself: a fresh mailbox
// carries the current index extensions, while an account that predates them
// takes a different path -- the one that was broken (#1184). So this check is
// only worth what the account is: point it at a long-lived user, never at one
// the smoke run provisions.
//
// Read-only by construction: ENVELOPE and BODYSTRUCTURE do not set \Seen, so
// a live mailbox is safe to probe. An empty INBOX gets a probe message that
// is removed afterwards.
func checkFetchEnvelope(user, pass string) error {
	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login %q: %w", user, err)
	}
	return fetchEnvelopeProbe(c)
}

// fetchEnvelopeProbe is checkFetchEnvelope with the connection already open,
// so the failure shapes can be driven in tests.
func fetchEnvelopeProbe(c *imapClient) error {
	exists, err := c.selectFolder("INBOX")
	if err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}
	if exists == 0 {
		const probe = "From: smoke@yarilo.test\r\n" +
			"Subject: " + fetchEnvelopeProbeSubject + "\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=\"sm0ke\"\r\n\r\n" +
			"--sm0ke\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
			"--sm0ke--\r\n"
		if err := c.append("INBOX", probe); err != nil {
			return fmt.Errorf("append probe to empty INBOX: %w", err)
		}
		defer func() {
			uids, serr := c.uidSearch("SUBJECT " + fetchEnvelopeProbeSubject)
			if serr == nil {
				_ = c.deleteUIDs(uids)
			}
		}()
		if exists, err = c.selectFolder("INBOX"); err != nil || exists == 0 {
			return fmt.Errorf("probe not visible in INBOX (exists=%d): %w", exists, err)
		}
	}

	// Twice: the first pass parses and fills the cache, the second is served
	// from it. A cache that answers wrongly, or a reader that dies on a cache
	// it just wrote, shows only on the second.
	// A range, not one message: a listing is a range, and a build that
	// answers the first row and dies on the fifth is the shape a single
	// fetch would call healthy.
	low := exists - 4
	if low < 1 {
		low = 1
	}
	want := exists - low + 1
	span := fmt.Sprintf("%d:%d", low, exists)

	for _, label := range []string{"cold", "warm"} {
		bad, err := fetchSpan(c, label, span, want)
		if err != nil {
			return err
		}
		if bad != "" {
			return fmt.Errorf("%s FETCH %s answered without %s; %s", label, span, bad, fetchRetries(c, label, span, want))
		}
	}
	return nil
}

// fetchSpan returns the first row missing an item, as "<item>: <row>".
func fetchSpan(c *imapClient, label, span string, want int) (string, error) {
	lines, ferr := c.cmd(fmt.Sprintf("FETCH %s (ENVELOPE BODYSTRUCTURE)", span))
	if ferr != nil {
		return "", fmt.Errorf("%s FETCH (ENVELOPE BODYSTRUCTURE) %s: %w", label, span, ferr)
	}
	// The row count, not a tagged OK: a partial answer is a tagged OK with
	// rows missing, and only counting tells it from a healthy run.
	rows := 0
	for _, l := range lines {
		if !strings.HasPrefix(l, "* ") || !strings.Contains(l, "FETCH") {
			continue
		}
		rows++
		for _, item := range []string{"ENVELOPE", "BODYSTRUCTURE"} {
			if !strings.Contains(l, item) {
				return item + ": " + l, nil
			}
		}
	}
	if rows != want {
		return "", fmt.Errorf("%s FETCH %s returned %d untagged rows, want %d", label, span, rows, want)
	}
	return "", nil
}

// fetchRetries asks again in the same session, then after a fresh SELECT, and
// says which answered: the first heals a load race, only the second a stale view.
func fetchRetries(c *imapClient, label, span string, want int) string {
	verdict := func(bad string, err error) string {
		switch {
		case err != nil:
			return "failed: " + err.Error()
		case bad != "":
			return "still without " + bad
		}
		return "answered"
	}
	time.Sleep(fetchRetryDelay)
	again := verdict(fetchSpan(c, label, span, want))
	reselect := "SELECT failed"
	if _, err := c.selectFolder("INBOX"); err == nil {
		reselect = verdict(fetchSpan(c, label, span, want))
	} else {
		reselect += ": " + err.Error()
	}
	return fmt.Sprintf("same session after %s: %s; after a fresh SELECT: %s", fetchRetryDelay, again, reselect)
}
