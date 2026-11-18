package sieve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/textproto"
	"strings"
	"sync"
	"time"
)

// redirectHandle keeps redirect records apart from a script's own duplicate
// handles in the store they share.
const redirectHandle = "vnd.yarilo.redirect"

// forwardStore is where a redirect records the forwards it made: the
// duplicate test's store, checked before a send and written after one.
type forwardStore interface {
	seen(ctx context.Context, id string) (bool, error)
	mark(ctx context.Context, id string, seconds uint32) error
}

// forwards picks the store by sieve_duplicate_driver, as dupTracker does.
func (e *Engine) forwards(username, homeDir string) forwardStore {
	switch e.cfg.DuplicateDriver {
	case "redis":
		if e.dupDict != nil {
			return NewDictDuplicateTracker(e.dupDict, username)
		}
	case "memory":
	default:
		if homeDir != "" {
			return NewFileDuplicateTracker(username, homeDir, e.cfg.DuplicateFile, e.locker)
		}
	}
	fresh := &memForwards{m: map[string]time.Time{}}
	v, _ := e.memForwards.LoadOrStore(username, fresh)
	if s, ok := v.(*memForwards); ok {
		return s
	}
	return fresh
}

// memForwards is the per-process store of the memory driver.
type memForwards struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func (s *memForwards) seen(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[id]
	return ok && time.Now().Before(exp), nil
}

func (s *memForwards) mark(_ context.Context, id string, seconds uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, exp := range s.m {
		if !now.Before(exp) {
			delete(s.m, k)
		}
	}
	s.m[id] = now.Add(time.Duration(seconds) * time.Second)
	return nil
}

func (t *FileDuplicateTracker) seen(ctx context.Context, id string) (bool, error) {
	key := duplicateID(redirectHandle, id)
	var found bool
	err := t.withLock(ctx, func(context.Context) error {
		records, err := t.readRecords()
		if err != nil {
			return err
		}
		now := uint32(time.Now().Unix())
		for _, r := range records {
			if r.id == key && r.expiresAt > now {
				found = true
			}
		}
		return nil
	})
	return found, err
}

func (t *FileDuplicateTracker) mark(ctx context.Context, id string, seconds uint32) error {
	key := duplicateID(redirectHandle, id)
	return t.withLock(ctx, func(context.Context) error {
		records, err := t.readRecords()
		if err != nil {
			return err
		}
		now := uint32(time.Now().Unix())
		active := records[:0]
		for _, r := range records {
			if r.expiresAt > now && r.id != key {
				active = append(active, r)
			}
		}
		return t.writeRecords(append(active, vacationRecord{expiresAt: now + seconds, id: key}))
	})
}

func (t *DictDuplicateTracker) seen(ctx context.Context, id string) (bool, error) {
	_, found, err := t.d.Lookup(ctx, nil, t.key(redirectHandle, id))
	return found, err
}

func (t *DictDuplicateTracker) mark(ctx context.Context, id string, seconds uint32) error {
	return t.mark0(ctx, t.key(redirectHandle, id), seconds)
}

// redirectOne sends one redirect, unless the message was already forwarded to
// that address (duplicate), carries this mailbox's trace header (loop) or is
// over the send limit; the checks run in that order, so a skipped redirect
// takes no slot of the limit. It reports whether the redirect went out; a
// send that failed did not.
func (e *Engine) redirectOne(ctx context.Context, opts FilterOptions, hdr textproto.MIMEHeader, to string) bool {
	msgID, newID := strings.TrimSpace(hdr.Get("Message-Id")), ""
	if msgID == "" {
		newID = newMessageID(opts.Username)
		msgID = newID
	}
	// A period of 0 remembers nothing: no record is read or written, since
	// a record without a lifetime would be kept for good.
	period := e.cfg.RedirectDuplicatePeriod
	id := forwardID(opts, hdr, msgID, to)
	var store forwardStore
	if period > 0 {
		store = e.forwards(opts.Username, opts.HomeDir)
		if dup, err := store.seen(ctx, id); err != nil {
			slog.Warn("sieve: redirect duplicate check failed, sending", "user", opts.Username, "to", to, "err", err)
		} else if dup {
			slog.Info("sieve: discarded duplicate redirect", "user", opts.Username, "to", to)
			return false
		}
	}
	if redirectLoop(opts, hdr) {
		slog.Warn("sieve: not redirecting, X-Sieve-Redirected-From shows a mail loop", "user", opts.Username, "to", to)
		return false
	}
	if !e.sendLimit.allow(ctx, opts.Username, opts.HomeDir, "redirect") {
		return false
	}
	raw := redirectMessage(opts.MsgRaw, opts.EnvTo, newID)
	// A send that failed is a redirect that did not go out: the message
	// stays, as for any other skipped redirect, rather than going nowhere.
	if err := e.sender.sendRedirect(ctx, opts.EnvFrom, to, raw); err != nil {
		slog.Error("sieve: redirect failed, message kept", "user", opts.Username, "to", to, "err", err)
		return false
	}
	if store != nil {
		if err := store.mark(ctx, id, uint32(period)); err != nil { //nolint:gosec
			slog.Warn("sieve: redirect duplicate record failed", "user", opts.Username, "to", to, "err", err)
		}
	}
	return true
}

// forwardID names one forward: the message, the mailbox that forwards it, the
// target, a resend's own id and the list it came through.
func forwardID(opts FilterOptions, hdr textproto.MIMEHeader, msgID, to string) string {
	rcpt := opts.OrigRcpt
	if rcpt == "" {
		rcpt = opts.EnvTo
	}
	resent := hdr.Get("Resent-Message-Id")
	if resent == "" {
		resent = hdr.Get("Resent-From")
	}
	return strings.Join([]string{msgID, rcpt, to, resent, hdr.Get("List-Id")}, "-")
}

// redirectLoop: a trace header naming this delivery's recipient or the user.
func redirectLoop(opts FilterOptions, hdr textproto.MIMEHeader) bool {
	for _, v := range hdr.Values("X-Sieve-Redirected-From") {
		v = strings.TrimSpace(v)
		if (opts.EnvTo != "" && v == opts.EnvTo) || (opts.Username != "" && v == opts.Username) {
			return true
		}
	}
	return false
}

// redirectMessage is the message as it is forwarded: X-Sieve and
// X-Sieve-Redirected-From prepended, a Message-ID when it had none,
// Return-Path removed, every other byte as it came.
func redirectMessage(raw []byte, from, newID string) []byte {
	var b bytes.Buffer
	b.WriteString("X-Sieve: yarilo\r\n")
	if from != "" {
		fmt.Fprintf(&b, "X-Sieve-Redirected-From: %s\r\n", oneLine(from))
	}
	if newID != "" {
		fmt.Fprintf(&b, "Message-ID: %s\r\n", newID)
	}
	b.Write(dropHeader(raw, "Return-Path"))
	return b.Bytes()
}

// dropHeader removes every field named name, continuation lines included,
// from the header section; the body is not looked at.
func dropHeader(raw []byte, name string) []byte {
	out := make([]byte, 0, len(raw))
	skipping := false
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, '\n')
		line := raw
		if end >= 0 {
			line = raw[:end+1]
		}
		raw = raw[len(line):]
		if bytes.Equal(bytes.TrimRight(line, "\r\n"), nil) {
			out = append(out, line...)
			return append(out, raw...)
		}
		if line[0] == ' ' || line[0] == '\t' {
			if !skipping {
				out = append(out, line...)
			}
			continue
		}
		colon := bytes.IndexByte(line, ':')
		skipping = colon > 0 && strings.EqualFold(strings.TrimSpace(string(line[:colon])), name)
		if !skipping {
			out = append(out, line...)
		}
	}
	return out
}

// newMessageID is an id for a message that had none, in the user's domain.
func newMessageID(user string) string {
	var r [12]byte
	_, _ = rand.Read(r[:])
	domain := "localhost"
	if i := strings.LastIndexByte(user, '@'); i >= 0 && i+1 < len(user) {
		domain = user[i+1:]
	}
	return fmt.Sprintf("<%s.%d@%s>", hex.EncodeToString(r[:]), time.Now().Unix(), domain)
}
