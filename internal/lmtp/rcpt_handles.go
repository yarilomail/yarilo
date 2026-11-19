package lmtp

import (
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// rcptHandlesMax bounds the recipients one connection keeps open: a relay's
// connection can carry many, and each holds its folders' parsed index.
const rcptHandlesMax = 16

// rcptHandles is one recipient's store and index, kept for the connection so
// the next message does not parse the folder's whole index again (#2618).
type rcptHandles struct {
	key  string
	box  mailbox.UserMailbox
	idx  mailbox.UserIndex
	mbox mailbox.Box
}

func (h *rcptHandles) close() {
	h.box.Close() //nolint:errcheck
	h.idx.Close() //nolint:errcheck
}

// rcptHandlesKey changes when the userdb moves the recipient's mail.
func rcptHandlesKey(ui *mailbox.UserInfo) string {
	return ui.Username + "\x00" + ui.Home + "\x00" + ui.MailPath + "\x00" + ui.Driver
}

// handlesFor returns the recipient's handles, opening them on first use and
// closing the one used longest ago past rcptHandlesMax.
func (s *session) handlesFor(ui *mailbox.UserInfo) *rcptHandles {
	key := rcptHandlesKey(ui)
	for i, h := range s.held {
		if h.key == key {
			s.held = append(append(s.held[:i:i], s.held[i+1:]...), h)
			return h
		}
	}
	mb := mailbox.SelectPersonalBackend(s.opts.Mailbox, s.opts.MailboxByDriver, ui.Driver)
	box := mb.OpenUser(ui)
	idx := s.opts.Index.OpenUser(ui)
	h := &rcptHandles{
		key: key, box: box, idx: idx,
		mbox: mailboxbase.Open(box, idx, mailboxbase.SaveOnly(), mailboxbase.WithAuto(s.personalAuto(ui))),
	}
	box.Init() //nolint:errcheck // idempotent; provisioned in rcptLocal
	if len(s.held) >= rcptHandlesMax {
		s.held[0].close()
		s.held = s.held[1:]
	}
	s.held = append(s.held, h)
	return h
}

// dropHandles closes a recipient's handles after a failed delivery, so the
// next message opens them afresh rather than on a state the failure left.
func (s *session) dropHandles(h *rcptHandles) {
	for i, held := range s.held {
		if held == h {
			h.close()
			s.held = append(s.held[:i:i], s.held[i+1:]...)
			return
		}
	}
}

// closeHeld releases every recipient's handles: the connection is over.
func (s *session) closeHeld() {
	for _, h := range s.held {
		h.close()
	}
	s.held = nil
}
