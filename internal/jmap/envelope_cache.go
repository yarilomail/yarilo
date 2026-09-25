package jmap

import (
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/msgcache"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// envelopeCaches holds one open cache per folder for the length of one
// Email/get. Opening per message would take the folder's lock -- a round trip
// to the lock service -- once per row of a listing, and hold it again on every
// close; the ids of one request also need not share a folder, hence a map
// rather than a single handle.
type envelopeCaches struct {
	s    *Server
	h    *userHandle
	open map[uint64]*msgcache.Handle
}

func (s *Server) newEnvelopeCaches(h *userHandle) *envelopeCaches {
	return &envelopeCaches{s: s, h: h, open: map[uint64]*msgcache.Handle{}}
}

// folder returns the folder's cache, opening it on first use. A nil *Handle is a
// working value meaning "no cache", so every caller degrades to parsing rather
// than to an error -- including the memoised nil of a folder that has none.
func (c *envelopeCaches) folder(ref messageRef) *msgcache.Handle {
	if c == nil {
		return nil
	}
	if fc, ok := c.open[ref.folderID]; ok {
		return fc
	}
	var fc *msgcache.Handle
	if c.h.idx != nil && c.s.opts.Storage != nil {
		fc = msgcache.Open(c.h.idx, ref.folderID, msgcache.Options{
			Locker:    c.s.opts.Storage.Locker,
			User:      c.h.info.Username,
			SessionID: c.h.info.SessionID,
			Folder:    ref.folder,
		})
	}
	c.open[ref.folderID] = fc
	return fc
}

func (c *envelopeCaches) Close() {
	if c == nil {
		return
	}
	for _, fc := range c.open {
		fc.Close()
	}
	clear(c.open)
}

// fillFromEnvelope answers the envelope-derived properties from a cached
// ENVELOPE, which is what lets a mailbox listing skip opening every message.
//
// It must agree with fillHeaders on the same message: the envelope was parsed
// from the same header block, so the difference is only where the parse
// happened. Fields ENVELOPE does not carry -- References, Headers -- stay
// empty, and EnvelopeSuffices refuses a request that names them.
//
// Message ids go through messageIDs for the same reason as the parsed path:
// JMAP carries them bare, without the angle brackets (§4.1.2.4).
func fillFromEnvelope(email *jmapcore.Email, env *imaplib.Envelope) {
	// The cache holds the header's own text, encoded words and all, because
	// that is what IMAP answers with; JMAP carries decoded text (#2008).
	if s := decodeWord(env.Subject); s != "" {
		email.Subject = &s
	}
	if !env.Date.IsZero() {
		s := env.Date.UTC().Format(time.RFC3339)
		email.SentAt = &s
	}
	email.MessageID = messageIDs(env.MessageID)
	email.InReplyTo = envInReplyTo(env.InReplyTo)
	email.From = envAddresses(env.From)
	email.Sender = envAddresses(env.Sender)
	email.To = envAddresses(env.To)
	email.CC = envAddresses(env.Cc)
	email.BCC = envAddresses(env.Bcc)
	email.ReplyTo = envAddresses(env.ReplyTo)
}

func envInReplyTo(ids []string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, messageIDs(id)...)
	}
	return out
}

// envAddresses converts ENVELOPE addresses to the JMAP form, decoding the
// display name: the cache carries the header's own text (#2008).
func envAddresses(addrs []imaplib.Address) []jmapcore.EmailAddress {
	if len(addrs) == 0 {
		return nil
	}
	out := make([]jmapcore.EmailAddress, 0, len(addrs))
	for _, a := range addrs {
		addr := jmapcore.EmailAddress{Email: a.Addr()}
		if a.Name != "" {
			name := decodeWord(a.Name)
			addr.Name = &name
		}
		out = append(out, addr)
	}
	return out
}
