package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	}
	// One deferred block, in the order the judgement needs: the folders go
	// first, and only then is the index asked what they left behind (#2022).
	defer func() {
		c.cmd("CLOSE") //nolint:errcheck
		for _, f := range []string{copyFolder, otherFolder} {
			if derr := c.deleteFolder(f); derr != nil && err == nil {
				err = derr
			}
		}
		if cerr := assertNoOrphanDocuments(user); cerr != nil && err == nil {
			err = cerr
		}
	}()

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

// assertNoOrphanDocuments judges the guarantee we chose: a retraction that was
// lost or raced is temporary, because a compaction clears it without anyone.
func assertNoOrphanDocuments(user string) error {
	docs, _, messages, err := pollFTSCounts(user)
	if err != nil {
		return err
	}
	if docs <= messages {
		return nil // nothing was left behind in the first place
	}
	// Said out loud, because a green row otherwise does not show which of the
	// two mechanisms held: the check at the write, or the sweep.
	slog.Info("smoke: a compaction was needed here",
		"user", user, "documents", docs, "live_messages", messages)
	if err := backendFTSOptimize(user); err != nil {
		return err
	}
	docs, _, messages, err = pollFTSCounts(user)
	if err != nil {
		return err
	}
	if docs > messages {
		return fmt.Errorf("a compaction left %d documents for %d live messages", docs, messages)
	}
	return nil
}

// pollFTSCounts reads the counts until they settle or the window runs out: the
// retractions this row is about run off the command path.
func pollFTSCounts(user string) (docs, copies, messages uint64, err error) {
	deadline := time.Now().Add(orphanCountWait)
	for {
		docs, copies, messages, err = backendFTSCounts(user)
		if err != nil || docs <= messages || time.Now().After(deadline) {
			return docs, copies, messages, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// backendFTSOptimize asks the backend to compact this account's index.
func backendFTSOptimize(user string) error {
	url := strings.TrimRight(*flagBackendAPI, "/") + "/api/backend/fts/optimize?user=" + user
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	if tok := backendAPIToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client, err := backendAPIClient()
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return explainBackendAPITransport(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fts/optimize: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

var orphanCountWait = 10 * time.Second

func backendFTSCounts(user string) (docs, copies, messages uint64, err error) {
	url := strings.TrimRight(*flagBackendAPI, "/") + "/api/backend/fts/status?user=" + user + "&folder=INBOX"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	if tok := backendAPIToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client, err := backendAPIClient()
	if err != nil {
		return 0, 0, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, explainBackendAPITransport(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, 0, 0, fmt.Errorf("fts/status: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Documents uint64 `json:"documents"`
		Copies    uint64 `json:"copies"`
		Messages  uint64 `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, 0, 0, fmt.Errorf("decode fts/status: %w", err)
	}
	return out.Documents, out.Copies, out.Messages, nil
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
	// Where it is, is a property of the Email: both mailboxes holding a copy
	// are named by one object (RFC 8621 §4).
	boxes, err := jmapMailboxIDsOf(user, ids[0])
	if err != nil {
		return err
	}
	if len(boxes) != copies {
		return fmt.Errorf("Email/get names %d mailboxes for %d live copies: %v", len(boxes), copies, boxes)
	}
	if !boxes[copyID] {
		return fmt.Errorf("Email/get does not name %q among the mailboxes holding the message", copyFolder)
	}
	// And only those: naming every mailbox would satisfy the count as well.
	if boxes[otherID] {
		return fmt.Errorf("Email/get names %q, a folder the message was never copied into", otherFolder)
	}
	return nil
}

// jmapMailboxIDsOf reads one Email's mailboxIds.
func jmapMailboxIDsOf(user, id string) (map[string]bool, error) {
	args, err := jmapCall(`{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[` +
		`["Email/get",{"accountId":"` + user + `","ids":["` + id + `"],"properties":["id","mailboxIds"]},"c0"]]}`)
	if err != nil {
		return nil, err
	}
	var out struct {
		List []struct {
			MailboxIDs map[string]bool `json:"mailboxIds"`
		} `json:"list"`
	}
	if err := json.Unmarshal(args, &out); err != nil {
		return nil, fmt.Errorf("decode Email/get: %w", err)
	}
	if len(out.List) != 1 {
		return nil, fmt.Errorf("Email/get returned %d objects for one id", len(out.List))
	}
	return out.List[0].MailboxIDs, nil
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
