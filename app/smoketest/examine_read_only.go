package main

import (
	"fmt"
	"strings"
)

// checkExamineReadOnly opens a folder of its own with one \Deleted message by
// EXAMINE: STORE and EXPUNGE must be refused and CLOSE must expunge nothing
// (RFC 9051 §6.3.3); the folder is removed afterwards.
func checkExamineReadOnly(user, pass string) error {
	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("imap dial: %w", err)
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("imap login: %w", err)
	}
	folder := "smoke-examine-" + uniqueID()
	if _, err := c.cmd(fmt.Sprintf("CREATE %q", folder)); err != nil {
		return fmt.Errorf("CREATE %q: %w", folder, err)
	}
	defer c.deleteFolder(folder) //nolint:errcheck
	body := "From: smoke@test.invalid\r\nSubject: " + folder + "\r\n\r\nx\r\n"
	if err := c.append(folder, body); err != nil {
		return fmt.Errorf("APPEND: %w", err)
	}
	if _, err := c.selectFolder(folder); err != nil {
		return fmt.Errorf("SELECT: %w", err)
	}
	if _, err := c.cmd(`STORE 1 +FLAGS.SILENT (\Deleted)`); err != nil {
		return fmt.Errorf("STORE \\Deleted under SELECT: %w", err)
	}
	if _, err := c.cmd(fmt.Sprintf("EXAMINE %q", folder)); err != nil {
		return fmt.Errorf("EXAMINE: %w", err)
	}
	for _, cmd := range []string{`STORE 1 +FLAGS (\Flagged)`, "EXPUNGE"} {
		_, err := c.cmd(cmd)
		if err == nil || !strings.Contains(err.Error(), " NO ") {
			return fmt.Errorf("%s under EXAMINE answered %v, want NO", cmd, err)
		}
	}
	if _, err := c.cmd("CLOSE"); err != nil {
		return fmt.Errorf("CLOSE: %w", err)
	}
	n, err := c.selectFolder(folder)
	if err != nil {
		return fmt.Errorf("SELECT after CLOSE: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("the folder holds %d messages after EXAMINE and CLOSE, want 1", n)
	}
	return nil
}
