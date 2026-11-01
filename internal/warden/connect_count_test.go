package warden

import (
	"errors"
	"net"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// countAtWrite records connect_total{res} at the moment the reply is written:
// a client reading the reply must already see the count (#2443).
type countAtWrite struct {
	net.Conn
	res    string
	writes int
	seen   float64
}

func (c *countAtWrite) Write(p []byte) (int, error) {
	c.writes++
	c.seen = testutil.ToFloat64(connectTotal.WithLabelValues(c.res))
	return len(p), nil
}

type connectErrState struct{ StateBackend }

func (connectErrState) SessionConnect(id, user, ip, service string) (bool, error) {
	return false, errors.New("redis down")
}

func TestConnectIsCountedBeforeTheReply(t *testing.T) {
	connect := []string{"CONNECT", "s1", "u@d", "10.0.0.1", "imap"}
	tests := []struct {
		name   string
		opts   []ServerOption
		full   bool // the user@ip slot is taken before the CONNECT
		line   []string
		res    string
		writes int
	}{
		{"ok", nil, false, connect, "ok", 1},
		{"refused", nil, true, connect, "too_many_connections", 1},
		{"state error", []ServerOption{WithStateBackend(connectErrState{})}, false, connect, "state_error", 1},
		{"bad request", nil, false, []string{"CONNECT", "s1", "u@d"}, "bad_request", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(1, tc.opts...)
			if tc.full {
				if ok, err := s.state.SessionConnect("s0", "u@d", "10.0.0.1", "imap"); !ok || err != nil {
					t.Fatalf("taking the slot: ok=%v err=%v", ok, err)
				}
			}
			before := testutil.ToFloat64(connectTotal.WithLabelValues(tc.res))
			c := &countAtWrite{res: tc.res}
			if got := s.handleConnect(c, tc.line); got != tc.res {
				t.Fatalf("result = %q, want %q", got, tc.res)
			}
			if c.writes != tc.writes {
				t.Fatalf("writes = %d, want %d", c.writes, tc.writes)
			}
			if tc.writes > 0 && c.seen != before+1 {
				t.Fatalf("connect_total{%s} at the reply = %v, want %v", tc.res, c.seen, before+1)
			}
			if got := testutil.ToFloat64(connectTotal.WithLabelValues(tc.res)); got != before+1 {
				t.Fatalf("connect_total{%s} = %v, want %v", tc.res, got, before+1)
			}
		})
	}
}
