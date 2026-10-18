package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// vacationScriptName is the managed script VacationResponse lives in (#713).
const vacationScriptName = "urn:ietf:params:jmap:vacationresponse"

// Row: a VacationResponse enabled over JMAP is ManageSieve's active script and
// answers a delivery; the probe's +detail sender brings the reply back to INBOX.
func checkConsistencyJMAPVacation(user, pass string) error {
	marker := consistencyMarker("jmapvacation")
	local, domain, ok := strings.Cut(user, "@")
	if !ok {
		return fmt.Errorf("account %q is not an address", user)
	}
	sender := local + "+" + marker + "@" + domain
	if err := jmapVacation(fmt.Sprintf(`{"isEnabled":true,"subject":"away %s","textBody":"I am away (%s)."}`, marker, marker)); err != nil {
		return err
	}
	defer cleanupJMAPVacation(user, pass, marker)

	listed, err := msieveListScripts()
	if err != nil {
		return fmt.Errorf("managesieve LISTSCRIPTS: %w", err)
	}
	if !strings.Contains(listed, fmt.Sprintf("%q ACTIVE", vacationScriptName)) {
		return fmt.Errorf("managesieve does not list %q as active: %q", vacationScriptName, listed)
	}
	raw := fmt.Sprintf("From: <%s>\r\nTo: <%s>\r\nSubject: probe %s\r\nMessage-ID: <%s@smoketest>\r\n\r\nvacation probe\r\n",
		sender, user, marker, uniqueID())
	if err := lmtpSendRaw(sender, user, raw); err != nil {
		return fmt.Errorf("deliver: %w", err)
	}
	return awaitVacationReply(user, pass, marker)
}

func jmapVacation(patch string) error {
	_, account, err := jmapLibClient()
	if err != nil {
		return err
	}
	args, err := jmapCall(fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:vacationresponse"],"methodCalls":[["VacationResponse/set",{"accountId":%q,"update":{"singleton":%s}},"0"]]}`,
		account, patch))
	if err != nil {
		return fmt.Errorf("VacationResponse/set: %w", err)
	}
	var resp struct {
		Updated    map[string]json.RawMessage `json:"updated"`
		NotUpdated map[string]json.RawMessage `json:"notUpdated"`
	}
	_ = json.Unmarshal(args, &resp)
	if _, ok := resp.Updated["singleton"]; !ok {
		return fmt.Errorf("VacationResponse/set did not update: %s", args)
	}
	return nil
}

// awaitVacationReply waits for the auto-reply: it answers the probe, so it
// carries the probe's subject after "away" and threads under it.
func awaitVacationReply(user, pass, marker string) error {
	c, err := imapDial()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	reply := "HEADER SUBJECT \"away " + marker + "\" HEADER AUTO-SUBMITTED auto-replied"
	for start := time.Now(); ; {
		if _, err := c.selectFolder("INBOX"); err != nil {
			return fmt.Errorf("select INBOX: %w", err)
		}
		uids, err := c.uidSearch(reply)
		if err != nil {
			return fmt.Errorf("uid search: %w", err)
		}
		if len(uids) > 0 {
			return nil
		}
		if time.Since(start) > *flagTimeout*3 {
			return fmt.Errorf("no vacation reply %q reached INBOX", "away "+marker)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// cleanupJMAPVacation disables the response, removes the managed script and
// both messages; best-effort, as for the other sieve rows.
func cleanupJMAPVacation(user, pass, marker string) {
	_ = jmapVacation(`{"isEnabled":false}`)
	if conn, err := msieveDial(); err == nil {
		defer conn.Close()
		if err := msieveAuth(conn, *flagManageSieveUser, *flagManageSievePass); err == nil {
			fmt.Fprintf(conn, "SETACTIVE \"\"\r\n")
			_, _ = readLine(conn)
			fmt.Fprintf(conn, "DELETESCRIPT %q\r\n", vacationScriptName)
			_, _ = readLine(conn)
		}
	}
	if c, err := imapDial(); err == nil {
		defer c.close()
		if err := c.login(user, pass); err == nil {
			if _, err := c.selectFolder("INBOX"); err == nil {
				if uids, err := c.uidSearch("TEXT " + marker); err == nil {
					_ = c.deleteUIDs(uids)
				}
			}
		}
	}
}
