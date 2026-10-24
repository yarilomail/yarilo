package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

const capWebSocketTicket = "com:linagora:params:jmap:ws:ticket"

// checkJMAPWebSocketTicket opens the WebSocket as a browser must: with a ticket
// from the session's generationEndpoint and no credentials, then sees a write
// pushed as a StateChange.
func checkJMAPWebSocketTicket() error {
	base := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort)
	endpoint, err := ticketEndpoint(base)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Duration(*flagJMAPPushPoll)*time.Second+*flagTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+endpoint, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(*flagJMAPUser, *flagJMAPPass)
	resp, err := jmapClient().Do(req)
	if err != nil {
		return fmt.Errorf("ticket: %w", err)
	}
	defer resp.Body.Close()
	var ticket struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&ticket); err != nil || resp.StatusCode != http.StatusOK || ticket.Value == "" {
		return fmt.Errorf("ticket: HTTP %d, %v", resp.StatusCode, err)
	}
	ws, _, err := websocket.Dial(ctx, "wss://"+net.JoinHostPort(jmapHost(), *flagJMAPPort)+"/jmap/ws/?ticket="+url.QueryEscape(ticket.Value), &websocket.DialOptions{ //nolint:bodyclose // the library owns the handshake response and nils its body
		HTTPClient:   &http.Client{Transport: &http.Transport{TLSClientConfig: publicTLS(jmapHost())}},
		Subprotocols: []string{"jmap"},
	})
	if err != nil {
		return fmt.Errorf("open websocket with the ticket alone: %w", err)
	}
	defer ws.CloseNow() //nolint:errcheck
	for _, msg := range []string{
		`{"@type":"WebSocketPushEnable","dataTypes":["Mailbox"]}`,
		`{"@type":"Request","id":"e","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c0"]]}`,
	} {
		if err := ws.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			return fmt.Errorf("websocket write: %w", err)
		}
	}
	if _, err := wsNextOf(ctx, ws, "Response"); err != nil {
		return fmt.Errorf("no Response to the echo: %w", err)
	}
	account := jmapAccountID()
	out, err := jmapCall(fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Mailbox/set",{"accountId":%q,"create":{"b":{"name":%q}}},"0"]]}`,
		account, "xwsticket-"+uniqueID()))
	if err != nil {
		return fmt.Errorf("Mailbox/set create: %w", err)
	}
	var created struct {
		Created map[string]struct{ ID string } `json:"created"`
	}
	_ = json.Unmarshal(out, &created)
	if id := created.Created["b"].ID; id != "" {
		defer jmapCall(fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Mailbox/set",{"accountId":%q,"destroy":[%q]},"0"]]}`, account, id)) //nolint:errcheck
	} else {
		return fmt.Errorf("Mailbox/set created nothing: %s", out)
	}
	data, err := wsNextOf(ctx, ws, "StateChange")
	if err != nil {
		return fmt.Errorf("no StateChange for the new mailbox: %w", err)
	}
	var change struct {
		Changed map[string]map[string]string `json:"changed"`
	}
	if err := json.Unmarshal(data, &change); err != nil || change.Changed[account]["Mailbox"] == "" {
		return fmt.Errorf("StateChange %s does not name the account's Mailbox state", data)
	}
	return nil
}

// ticketEndpoint reads the ticket's path from the session, where the account
// must carry the capability too.
func ticketEndpoint(base string) (string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/.well-known/jmap", nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(*flagJMAPUser, *flagJMAPPass)
	resp, err := jmapClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("session: %w", err)
	}
	defer resp.Body.Close()
	var session struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Accounts     map[string]struct {
			AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&session); err != nil {
		return "", fmt.Errorf("session: %w", err)
	}
	var cap struct {
		GenerationEndpoint string `json:"generationEndpoint"`
	}
	_ = json.Unmarshal(session.Capabilities[capWebSocketTicket], &cap)
	acc := session.Accounts[jmapAccountID()].AccountCapabilities
	if _, ok := acc[capWebSocketTicket]; !ok || cap.GenerationEndpoint == "" {
		return "", fmt.Errorf("the session does not advertise %s at the session and the account", capWebSocketTicket)
	}
	if _, ok := acc["urn:ietf:params:jmap:websocket"]; !ok {
		return "", fmt.Errorf("the account does not carry urn:ietf:params:jmap:websocket")
	}
	u, err := url.Parse(cap.GenerationEndpoint)
	if err != nil {
		return "", fmt.Errorf("generationEndpoint %q: %w", cap.GenerationEndpoint, err)
	}
	return u.Path, nil
}
