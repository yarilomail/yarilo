package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// checkConsistencyPush sets a flag over IMAP under an open EventSource: a
// StateChange naming Email must arrive within two poll intervals.
func checkConsistencyPush(user, pass string) error {
	marker := consistencyMarker("push")
	if err := deliverConsistencyProbe(user, marker); err != nil {
		return err
	}
	c, err := imapDial()
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if _, err := c.selectFolder("INBOX"); err != nil {
		return fmt.Errorf("select INBOX: %w", err)
	}
	uid, err := waitForIMAPProbe(c, marker)
	if err != nil {
		return err
	}

	budget := 2*time.Duration(*flagJMAPPushPoll)*time.Second + *flagTimeout
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	url := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort) + "/jmap/eventsource/?types=Email&closeafter=state&ping=0"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(*flagJMAPUser, *flagJMAPPass)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: publicTLS(jmapHost())}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("open eventsource: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("eventsource answered HTTP %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return fmt.Errorf("eventsource Content-Type %q, want text/event-stream", ct)
	}

	if _, err := c.cmd(fmt.Sprintf("UID STORE %s +FLAGS (%s)", uid, consistencyCustomFlag)); err != nil {
		return fmt.Errorf("uid store: %w", err)
	}
	sc := bufio.NewScanner(resp.Body)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			event = v
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || event != "state" {
			continue
		}
		var change struct {
			Type    string                       `json:"@type"`
			Changed map[string]map[string]string `json:"changed"`
		}
		if err := json.Unmarshal([]byte(data), &change); err != nil {
			return fmt.Errorf("decode StateChange: %w", err)
		}
		if change.Type != "StateChange" || change.Changed[*flagJMAPUser]["Email"] == "" {
			return fmt.Errorf("event %s does not name the account's Email state", data)
		}
		return nil
	}
	return fmt.Errorf("no StateChange within %v of the flag change: %v", budget, sc.Err())
}
