package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

// checkJMAPSubmission is the #713 acceptance: a draft sent with
// EmailSubmission/set reaches the mailbox through the relay, without its Bcc.
func checkJMAPSubmission() error {
	c, account, err := jmapLibClient()
	if err != nil {
		return err
	}
	user := *flagJMAPUser
	identity, err := jmapIdentityOf(account, user)
	if err != nil {
		return err
	}
	marker := "xjmapsubmit-" + uniqueID()
	raw := fmt.Sprintf("From: <%s>\r\nTo: <%s>\r\nBcc: <%s>\r\nSubject: %s\r\nMessage-ID: <%s@smoketest>\r\n\r\nsent with EmailSubmission\r\n",
		user, user, user, marker, marker)
	up, err := c.Upload(account, strings.NewReader(raw))
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	box, err := jmapInboxID()
	if err != nil {
		return err
	}
	imported, err := jmapCall(fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Email/import",{"accountId":%q,"emails":{"d":{"blobId":%q,"mailboxIds":{%q:true},"keywords":{"$draft":true}}}},"0"]]}`,
		account, up.ID, box))
	if err != nil {
		return fmt.Errorf("Email/import of the draft: %w", err)
	}
	var imp struct {
		Created map[string]struct{ ID string } `json:"created"`
	}
	_ = json.Unmarshal(imported, &imp)
	draft := imp.Created["d"].ID
	if draft == "" {
		return fmt.Errorf("Email/import created no draft: %s", imported)
	}
	defer jmapCall(fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Email/set",{"accountId":%q,"destroy":[%q]},"0"]]}`, account, draft)) //nolint:errcheck

	set, err := jmapCall(fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],"methodCalls":[["EmailSubmission/set",{"accountId":%q,"create":{"c1":{"identityId":%q,"emailId":%q}},"onSuccessUpdateEmail":{"#c1":{"keywords/$draft":null}}},"0"]]}`,
		account, identity, draft))
	if err != nil {
		return fmt.Errorf("EmailSubmission/set: %w", err)
	}
	var sub struct {
		Created map[string]struct {
			UndoStatus string `json:"undoStatus"`
		} `json:"created"`
	}
	_ = json.Unmarshal(set, &sub)
	if sub.Created["c1"].UndoStatus != "final" {
		return fmt.Errorf("EmailSubmission/set sent nothing: %s", set)
	}
	return arrivedWithoutBcc(user, *flagJMAPPass, marker)
}

// arrivedWithoutBcc waits for the delivered copy (the draft is in the same
// INBOX, so it is told apart by having passed the relay) and requires no Bcc.
func arrivedWithoutBcc(user, pass, marker string) error {
	c, err := imapDial()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	delivered := "HEADER SUBJECT " + marker + " HEADER RECEIVED relay"
	for start := time.Now(); ; {
		if _, err := c.selectFolder("INBOX"); err != nil {
			return fmt.Errorf("select INBOX: %w", err)
		}
		uids, err := c.uidSearch(delivered)
		if err != nil {
			return fmt.Errorf("uid search: %w", err)
		}
		if len(uids) > 0 {
			withBcc, err := c.uidSearch(delivered + ` HEADER BCC ""`)
			_ = c.deleteUIDs(uids)
			if err != nil {
				return fmt.Errorf("uid search for Bcc: %w", err)
			}
			if len(withBcc) > 0 {
				return fmt.Errorf("the delivered message still carries its Bcc header")
			}
			return nil
		}
		if time.Since(start) > *flagTimeout*3 {
			return fmt.Errorf("the submitted message %s did not reach INBOX through the relay", marker)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func jmapIdentityOf(account gojmap.ID, user string) (string, error) {
	out, err := jmapCall(fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:submission"],"methodCalls":[["Identity/get",{"accountId":%q,"ids":null},"0"]]}`, account))
	if err != nil {
		return "", fmt.Errorf("Identity/get: %w", err)
	}
	var got struct {
		List []struct{ ID, Email string } `json:"list"`
	}
	_ = json.Unmarshal(out, &got)
	for _, id := range got.List {
		if strings.EqualFold(id.Email, user) {
			return id.ID, nil
		}
	}
	return "", fmt.Errorf("no identity for %s among %v", user, got.List)
}
