package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Row: a script stored and activated over JMAP is the active script ManageSieve
// lists, and it takes effect on the next delivery (#713).
const consistencyJMAPSieveScript = "smoke-jmap-sieve"

func checkConsistencyJMAPSieve(user, pass string) error {
	marker := consistencyMarker("jmapsieve")
	folder := "consistency-" + marker
	if err := createFolder(user, pass, folder); err != nil {
		return fmt.Errorf("pre-create %s: %w", folder, err)
	}
	script := fmt.Sprintf("require [\"fileinto\"];\r\nif header :contains \"Subject\" %q { fileinto %q; }\r\n",
		marker, folder)
	if err := jmapPutAndActivateSieve(consistencyJMAPSieveScript, script); err != nil {
		return err
	}
	defer cleanupJMAPSieve(user, pass, folder)

	listed, err := msieveListScripts()
	if err != nil {
		return fmt.Errorf("managesieve LISTSCRIPTS: %w", err)
	}
	if !strings.Contains(listed, fmt.Sprintf("%q ACTIVE", consistencyJMAPSieveScript)) {
		return fmt.Errorf("managesieve does not list %q as active: %q", consistencyJMAPSieveScript, listed)
	}
	if err := deliverConsistencyProbe(user, marker); err != nil {
		return err
	}
	return assertFiledInto(user, pass, folder, marker)
}

func jmapPutAndActivateSieve(name, body string) error {
	c, account, err := jmapLibClient()
	if err != nil {
		return err
	}
	up, err := c.Upload(account, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	req := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:sieve"],"methodCalls":[["SieveScript/set",{"accountId":%q,"create":{"c1":{"name":%q,"blobId":%q}},"onSuccessActivateScript":"#c1"},"0"]]}`,
		account, name, up.ID)
	args, err := jmapCall(req)
	if err != nil {
		return fmt.Errorf("SieveScript/set: %w", err)
	}
	var resp struct {
		Created    map[string]struct{ IsActive bool } `json:"created"`
		NotCreated map[string]json.RawMessage         `json:"notCreated"`
	}
	if err := json.Unmarshal(args, &resp); err != nil {
		return fmt.Errorf("SieveScript/set response: %w", err)
	}
	if c1, ok := resp.Created["c1"]; !ok || !c1.IsActive {
		return fmt.Errorf("SieveScript/set did not create an active script: %s", args)
	}
	return nil
}

// msieveListScripts returns LISTSCRIPTS as the server sent it.
func msieveListScripts() (string, error) {
	conn, err := msieveDial()
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := msieveAuth(conn, *flagManageSieveUser, *flagManageSievePass); err != nil {
		return "", err
	}
	fmt.Fprintf(conn, "LISTSCRIPTS\r\n")
	var b strings.Builder
	for {
		line, err := readLine(conn)
		if err != nil {
			return b.String(), err
		}
		if strings.HasPrefix(line, "OK") {
			return b.String(), nil
		}
		if strings.HasPrefix(line, "NO") || strings.HasPrefix(line, "BYE") {
			return b.String(), fmt.Errorf("refused: %q", line)
		}
		b.WriteString(line + "\n")
	}
}

// cleanupJMAPSieve deactivates and removes the script and the folder;
// best-effort, as for the ManageSieve row.
func cleanupJMAPSieve(user, pass, folder string) {
	if conn, err := msieveDial(); err == nil {
		defer conn.Close()
		if err := msieveAuth(conn, *flagManageSieveUser, *flagManageSievePass); err == nil {
			fmt.Fprintf(conn, "SETACTIVE \"\"\r\n")
			_, _ = readLine(conn)
			fmt.Fprintf(conn, "DELETESCRIPT %q\r\n", consistencyJMAPSieveScript)
			_, _ = readLine(conn)
		}
	}
	if c, err := imapDial(); err == nil {
		defer c.close()
		if err := c.login(user, pass); err == nil {
			_ = c.deleteFolder(folder)
		}
	}
}
