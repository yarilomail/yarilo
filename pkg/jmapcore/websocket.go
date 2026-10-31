package jmapcore

import "strings"

// CapWebSocket is JMAP over WebSocket (RFC 8887).
const CapWebSocket = "urn:ietf:params:jmap:websocket"

// WebSocketPath is the WebSocket endpoint.
const WebSocketPath = "/jmap/ws/"

// CapWebSocketTicket is one web client's own extension, the only vendor name in
// this protocol, by decision: a browser cannot send credentials on a WebSocket.
const CapWebSocketTicket = "com:linagora:params:jmap:ws:ticket"

// WebSocketTicketPath issues a WebSocket ticket on POST; jmap-login serves it.
const WebSocketTicketPath = "/jmap/ws/ticket"

// WebSocketTicketCapability names where a ticket is issued.
type WebSocketTicketCapability struct {
	GenerationEndpoint string `json:"generationEndpoint"`
}

// WebSocketCapability is the urn:ietf:params:jmap:websocket object
// (RFC 8887 §4.1). BuildSession fills URL from the base URL.
type WebSocketCapability struct {
	URL          string `json:"url"`
	SupportsPush bool   `json:"supportsPush"`
}

// webSocketURL turns the public origin into the WebSocket one.
func webSocketURL(base string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base + WebSocketPath
}
