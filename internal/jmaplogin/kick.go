package jmaplogin

import (
	"context"
	"crypto/tls"
	"log/slog"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/cluster/proto"
	"github.com/yarilomail/yarilo/v2/internal/warden"
)

// kickChannel is warden's channel for this proxy's kicks; the payload is the
// session id the connection was accounted under.
const kickChannel = "kick:" + service

// eventSourcePath is the backend's push stream (RFC 8620 §7.3).
const eventSourcePath = "/jmap/eventsource/"

// kickRetry is the pause before a dropped kick source is dialled again.
const kickRetry = 2 * time.Second

// Kicks are where kicks arrive from; an empty address leaves that source off.
type Kicks struct {
	WardenAddr   string
	WardenTLS    *tls.Config
	DirectorAddr string
	DirectorTLS  *tls.Config
	LocalIP      string
}

// kickUser closes every connection accounted as user: a push stream is one of
// them, and HTTP has nothing else that lives long enough to kick.
func (s *Server) kickUser(user, reason string) int {
	n := 0
	s.conns.Range(func(_, v any) bool {
		st, ok := v.(*connState)
		if !ok {
			return true
		}
		st.mu.Lock()
		match := st.user == user && st.conn != nil
		c := st.conn
		st.mu.Unlock()
		if match {
			_ = c.Close()
			n++
		}
		return true
	})
	slog.Info("jmap-login: user kick received", "user", user, "reason", reason, "connections", n)
	return n
}

// kickSession closes the connection accounted under id, if this pod has it.
func (s *Server) kickSession(id string) bool {
	found := false
	s.conns.Range(func(_, v any) bool {
		st, ok := v.(*connState)
		if !ok {
			return true
		}
		st.mu.Lock()
		match := st.id == id && st.conn != nil
		c := st.conn
		st.mu.Unlock()
		if match {
			_ = c.Close()
			found = true
			return false
		}
		return true
	})
	return found
}

// WatchKicks listens to warden and the director until ctx ends.
func (s *Server) WatchKicks(ctx context.Context, k Kicks) {
	if k.WardenAddr != "" {
		go s.wardenKicks(ctx, k)
	}
	if k.DirectorAddr != "" {
		go s.directorKicks(ctx, k)
	}
}

func (s *Server) wardenKicks(ctx context.Context, k Kicks) {
	for ctx.Err() == nil {
		ac, err := warden.Dial(k.WardenAddr, k.WardenTLS, 5*time.Second)
		if err == nil {
			var ch <-chan string
			if ch, err = ac.Subscribe(ctx, kickChannel); err == nil {
				for id := range ch {
					s.kickSession(id)
				}
			}
			ac.Close()
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("jmap-login: warden kick subscription down, retrying", "err", err)
		sleep(ctx, kickRetry)
	}
}

func (s *Server) directorKicks(ctx context.Context, k Kicks) {
	for ctx.Err() == nil {
		var c *proto.Conn
		var err error
		if k.DirectorTLS != nil {
			c, err = proto.DialTLS(k.DirectorAddr, k.LocalIP, 0, k.DirectorTLS)
		} else {
			c, err = proto.Dial(k.DirectorAddr, k.LocalIP, 0)
		}
		if err == nil {
			stop := context.AfterFunc(ctx, func() { _ = c.Close() })
			err = s.readKicks(c)
			stop()
			_ = c.Close()
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("jmap-login: director kick watch down, retrying", "err", err)
		sleep(ctx, kickRetry)
	}
}

// readKicks serves the director's pushes: USER-KICKED closes the user's
// connections, whatever the reason, and PING is answered.
func (s *Server) readKicks(c *proto.Conn) error {
	for {
		line, err := c.ReadLine()
		if err != nil {
			return err
		}
		switch {
		case line == "PING":
			_ = c.WriteLine("PONG")
		case strings.HasPrefix(line, "USER-KICKED\t"):
			f := strings.Split(line, "\t")
			if len(f) < 2 || len(f) > 3 || f[1] == "" {
				slog.Warn("jmap-login: malformed USER-KICKED push, ignored", "line", line)
				continue
			}
			reason := ""
			if len(f) == 3 {
				reason, _ = strings.CutPrefix(f[2], proto.KickReasonField)
			}
			s.kickUser(f[1], reason)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
