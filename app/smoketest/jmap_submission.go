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
	// One request, as clients send: the draft and its submission by "#d" (RFC 8620 §5.3).
	all, err := jmapCallAll(fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],"methodCalls":[`+
		`["Email/import",{"accountId":%q,"emails":{"d":{"blobId":%q,"mailboxIds":{%q:true},"keywords":{"$draft":true}}}},"0"],`+
		`["EmailSubmission/set",{"accountId":%q,"create":{"c1":{"identityId":%q,"emailId":"#d"}},"onSuccessDestroyEmail":["#c1"]},"1"]]}`,
		account, up.ID, box, account, identity))
	if err != nil {
		return fmt.Errorf("Email/import and EmailSubmission/set: %w", err)
	}
	var imp struct {
		Created map[string]struct{ ID string } `json:"created"`
	}
	var sub struct {
		Created map[string]struct {
			UndoStatus string `json:"undoStatus"`
		} `json:"created"`
	}
	var destroyed struct {
		Destroyed []string `json:"destroyed"`
	}
	if len(all) != 3 || all[0].name != "Email/import" || all[1].name != "EmailSubmission/set" || all[2].name != "Email/set" {
		return fmt.Errorf("responses %v, want Email/import, EmailSubmission/set and its implicit Email/set", all)
	}
	_ = json.Unmarshal(all[0].args, &imp)
	_ = json.Unmarshal(all[1].args, &sub)
	_ = json.Unmarshal(all[2].args, &destroyed)
	draft := imp.Created["d"].ID
	if draft == "" {
		return fmt.Errorf("Email/import created no draft: %s", all[0].args)
	}
	if sub.Created["c1"].UndoStatus != "final" {
		jmapCall(fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Email/set",{"accountId":%q,"destroy":[%q]},"0"]]}`, account, draft)) //nolint:errcheck
		return fmt.Errorf("EmailSubmission/set with emailId #d sent nothing: %s", all[1].args)
	}
	if len(destroyed.Destroyed) != 1 || destroyed.Destroyed[0] != draft {
		return fmt.Errorf("the implicit Email/set did not destroy the draft %s: %s", draft, all[2].args)
	}
	return arrivedWithoutBcc(user, *flagJMAPPass, marker, "")
}

// checkJMAPCreateAndSend sends as a web client does: Email/set creates the
// message with a header:* property and EmailSubmission/set sends it by "#m".
func checkJMAPCreateAndSend() error {
	_, account, err := jmapLibClient()
	if err != nil {
		return err
	}
	user := *flagJMAPUser
	identity, err := jmapIdentityOf(account, user)
	if err != nil {
		return err
	}
	box, err := jmapInboxID()
	if err != nil {
		return err
	}
	marker := "xjmapcreate-" + uniqueID()
	all, err := jmapCallAll(fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],"methodCalls":[`+
		`["Email/set",{"accountId":%q,"create":{"m":{"mailboxIds":{%q:true},"keywords":{"$draft":true},"from":[{"email":%q}],"to":[{"email":%q}],"subject":%q,`+
		`"header:User-Agent:asText":%q,"htmlBody":[{"partId":"h","type":"text/html"}],"bodyValues":{"h":{"value":"<p>created with Email/set</p>"}}}}},"0"],`+
		`["EmailSubmission/set",{"accountId":%q,"create":{"c1":{"identityId":%q,"emailId":"#m"}},"onSuccessDestroyEmail":["#c1"]},"1"]]}`,
		account, box, user, user, marker, marker, account, identity))
	if err != nil {
		return fmt.Errorf("Email/set and EmailSubmission/set: %w", err)
	}
	var created struct {
		Created    map[string]struct{ ID string } `json:"created"`
		NotCreated map[string]any                 `json:"notCreated"`
	}
	if len(all) < 2 || all[0].name != "Email/set" || all[1].name != "EmailSubmission/set" {
		return fmt.Errorf("responses %v, want Email/set and EmailSubmission/set", all)
	}
	_ = json.Unmarshal(all[0].args, &created)
	if created.Created["m"].ID == "" {
		return fmt.Errorf("Email/set with header:User-Agent:asText created nothing: %v", created.NotCreated)
	}
	var sub struct {
		Created map[string]struct {
			UndoStatus string `json:"undoStatus"`
		} `json:"created"`
	}
	_ = json.Unmarshal(all[1].args, &sub)
	if sub.Created["c1"].UndoStatus != "final" {
		jmapCall(fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Email/set",{"accountId":%q,"destroy":[%q]},"0"]]}`, account, created.Created["m"].ID)) //nolint:errcheck
		return fmt.Errorf("EmailSubmission/set with emailId #m sent nothing: %s", all[1].args)
	}
	return arrivedWithoutBcc(user, *flagJMAPPass, marker, " HEADER USER-AGENT "+marker)
}

// arrivedWithoutBcc waits for the delivered copy (the draft is in the same
// INBOX, so it is told apart by having passed the relay) and requires no Bcc;
// also is more search criteria the copy must match.
func arrivedWithoutBcc(user, pass, marker, also string) error {
	c, err := imapDial()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	delivered := "HEADER SUBJECT " + marker + " HEADER RECEIVED relay" + also
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
