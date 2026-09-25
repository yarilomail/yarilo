package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// checkFTSDocumentIsMessage proves on real storage what the engine rows prove
// at the seam: the indexed document is the message, not the copy (#1986).
func checkFTSDocumentIsMessage(user, pass string, withJMAP bool) (err error) {
	stamp := time.Now().UnixNano() % 1e9
	marker := fmt.Sprintf("dfts%09dhit", stamp)
	subject := "fts copy smoke " + marker
	copyFolder := fmt.Sprintf("SmokeFTSCopy%d", stamp)
	otherFolder := fmt.Sprintf("SmokeFTSOther%d", stamp)

	if err := lmtpSend(uniqueID(), "fts-copy-probe@test.invalid", user,
		subject, "the fts copy probe body "+marker+" end"); err != nil {
		return fmt.Errorf("deliver: %w", err)
	}

	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login %q: %w", user, err)
	}
	for _, folder := range []string{copyFolder, otherFolder} {
		if _, err := c.cmd(fmt.Sprintf("CREATE %q", folder)); err != nil {
			return fmt.Errorf("CREATE %q: %w", folder, err)
		}
		// The folders this run created go, and a failure to remove one is
		// reported rather than left for the next run to trip over.
		defer func(f string) {
			c.cmd("CLOSE") //nolint:errcheck
			if derr := c.deleteFolder(f); derr != nil && err == nil {
				err = derr
			}
		}(folder)
	}

	if _, err := c.selectFolder("INBOX"); err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}
	uids, err := waitForHits(c, marker, 1)
	if err != nil {
		return fmt.Errorf("INBOX: %w", err)
	}
	if _, err := c.cmd(fmt.Sprintf("UID COPY %s %q", strings.Join(uids, ","), copyFolder)); err != nil {
		return fmt.Errorf("UID COPY to %q: %w", copyFolder, err)
	}

	if _, err := c.selectFolder(copyFolder); err != nil {
		return fmt.Errorf("select %q: %w", copyFolder, err)
	}
	copyUIDs, err := waitForHits(c, marker, 1)
	if err != nil {
		return fmt.Errorf("%s: %w", copyFolder, err)
	}
	// The copy is indexed under its own folder and nowhere else: a folder the
	// message never reached must answer with nothing.
	if err := assertHits(c, otherFolder, marker, 0); err != nil {
		return err
	}

	// While both copies are alive: two copies of one message answer under one
	// emailId, which an expunged one could not distinguish (RFC 8474 §5.1).
	if withJMAP {
		if err := assertOneEmailIDForCopies(user, marker, copyFolder, otherFolder, len(copyUIDs)+1); err != nil {
			return err
		}
	}

	if _, err := c.selectFolder("INBOX"); err != nil {
		return fmt.Errorf("re-select INBOX: %w", err)
	}
	if err := c.deleteUIDs(uids); err != nil {
		return fmt.Errorf("expunge the INBOX copy: %w", err)
	}
	if err := assertHits(c, "INBOX", marker, 0); err != nil {
		return err
	}
	// The surviving copy is the point: dropping one copy must retract that
	// folder's terms, not the document.
	if err := assertHits(c, copyFolder, marker, 1); err != nil {
		return err
	}
	return nil
}

// The judgement that separates "document = the message" from "document = the
// copy": two copies answer under one emailId, narrowed by the folder.
func assertOneEmailIDForCopies(user, marker, copyFolder, otherFolder string, copies int) error {
	ids, _, err := jmapQueryText(user, marker)
	if err != nil {
		return fmt.Errorf("Email/query text %q: %w", marker, err)
	}
	if len(ids) != 1 {
		return fmt.Errorf("Email/query text %q returned %d ids for %d copies of one message, want 1",
			marker, len(ids), copies)
	}
	copyID, err := jmapMailboxID(copyFolder)
	if err != nil {
		return err
	}
	otherID, err := jmapMailboxID(otherFolder)
	if err != nil {
		return err
	}
	in, err := jmapQueryTextIn(user, marker, copyID)
	if err != nil {
		return fmt.Errorf("Email/query inMailbox %q: %w", copyFolder, err)
	}
	if len(in) != 1 || in[0] != ids[0] {
		return fmt.Errorf("Email/query inMailbox %q returned %v, want the single id %v", copyFolder, in, ids)
	}
	out, err := jmapQueryTextIn(user, marker, otherID)
	if err != nil {
		return fmt.Errorf("Email/query inMailbox %q: %w", otherFolder, err)
	}
	if len(out) != 0 {
		return fmt.Errorf("Email/query inMailbox %q returned %d ids for a folder the message was never copied into",
			otherFolder, len(out))
	}
	return nil
}

// waitForHits waits out the asynchronous indexing for the selected folder and
// returns the UIDs once the expected number of hits is there.
func waitForHits(c *imapClient, marker string, want int) ([]string, error) {
	deadline := time.Now().Add(*flagTimeout * 3)
	var last error
	for time.Now().Before(deadline) {
		uids, err := c.uidSearch(fmt.Sprintf("BODY %q", marker))
		switch {
		case err != nil:
			last = fmt.Errorf("SEARCH BODY %q: %w", marker, err)
		case len(uids) == want:
			return uids, nil
		default:
			last = fmt.Errorf("SEARCH BODY %q found %d messages, want %d", marker, len(uids), want)
		}
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("indexing never settled: %w", last)
}

// assertHits selects a folder and requires exactly want matches there.
func assertHits(c *imapClient, folder, marker string, want int) error {
	if _, err := c.selectFolder(folder); err != nil {
		return fmt.Errorf("select %q: %w", folder, err)
	}
	if _, err := waitForHits(c, marker, want); err != nil {
		return fmt.Errorf("%s: %w", folder, err)
	}
	return nil
}

// jmapQueryTextIn runs Email/query with a text condition scoped to one
// mailbox.
func jmapQueryTextIn(user, text, mailboxID string) ([]string, error) {
	filter, err := json.Marshal(map[string]string{"text": text, "inMailbox": mailboxID})
	if err != nil {
		return nil, err
	}
	args, err := jmapCall(`{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[` +
		`["Email/query",{"accountId":"` + user + `","filter":` + string(filter) + `},"c0"]]}`)
	if err != nil {
		return nil, err
	}
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(args, &out); err != nil {
		return nil, fmt.Errorf("decode Email/query: %w", err)
	}
	return out.IDs, nil
}

type ftsAccount struct{ user, pass string }

// ftsCopyAccounts reads -fts-copy-users, falling back to the single -fts-user.
func ftsCopyAccounts() []ftsAccount {
	if strings.TrimSpace(*flagFTSCopyUsers) == "" {
		if *flagFTSUser == "" {
			return nil
		}
		return []ftsAccount{{*flagFTSUser, *flagFTSPass}}
	}
	var out []ftsAccount
	for _, entry := range strings.Split(*flagFTSCopyUsers, ",") {
		user, pass, _ := strings.Cut(strings.TrimSpace(entry), ":")
		if user != "" {
			out = append(out, ftsAccount{user, pass})
		}
	}
	return out
}
