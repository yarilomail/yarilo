package jmap

import "github.com/yarilomail/yarilo/pkg/mailbox"

// index is the handle's index, for a test that plays another session on it;
// the code under test reaches it only through Box.
func (h *userHandle) index() mailbox.UserIndex {
	return h.mbox.(interface{ Index() mailbox.UserIndex }).Index()
}
