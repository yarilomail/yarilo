package imap

import (
	"log/slog"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/locks"
)

// afterCreate gives a new folder its ACL: inheritance is materialised so the
// creator, often holding create only at the root, is named in it (#1111).
func (s *session) afterCreate(h *nsHandle, rel, name string) error {
	if h.acl == nil || !s.aclEnforced(h) {
		return nil
	}
	if err := h.acl.MaterialiseOnCreate(rel); err != nil {
		slog.Warn("imap: acl inheritance not materialised", "folder", name, "err", err)
	}
	if err := s.grantCreatorAdmin(h, rel); err != nil {
		return s.rollBackUnadministered(h, rel, name, err)
	}
	return nil
}

// boxAuto is h's configured mailboxes with what a session adds to one the Box
// makes: the ACL, the list event, and the subscription.
func (s *session) boxAuto(h *nsHandle) mailboxbase.Auto {
	return mailboxbase.Auto{
		Mailboxes: h.spec.Mailboxes,
		Limit:     s.srv.opts.QuotaPolicy.MailboxCount,
		Created: func(rel string) {
			if err := s.afterCreate(h, rel, h.fullName(rel)); err != nil {
				slog.Warn("imap: configured mailbox left without its acl", "folder", h.fullName(rel), "err", err)
			}
			s.emitMailboxList(locks.EventMailboxCreate, h.fullName(rel))
		},
		Subscribe: func(rel string) error {
			store, keyPrefix, err := s.subsView(h)
			if err != nil {
				return err
			}
			_, err = store.AddOwn(keyPrefix + rel)
			return err
		},
	}
}
