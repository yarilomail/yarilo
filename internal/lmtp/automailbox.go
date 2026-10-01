package lmtp

import (
	"context"
	"log/slog"
	"time"

	"github.com/yarilomail/yarilo/internal/mailboxcreate"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/internal/userstate/subs"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// boxAuto is namespace i's configured mailboxes for a delivery to personal,
// opened as nsUI: a delivery opens a folder as any server does (#2005).
func (s *session) boxAuto(personal *mailbox.UserInfo, i int, nsUI *mailbox.UserInfo) mailboxbase.Auto {
	if i < 0 || i >= len(s.opts.Namespaces) {
		// No namespaces block: the personal store and its one file.
		return mailboxbase.Auto{Subscribe: func(rel string) error {
			owner := locks.Owner(personal.Username, s.stampLockID(personal).LockID())
			_, err := subs.New(mailbox.ControlRoot(personal), "subscriptions", personal.Username, owner, s.opts.Locker).AddOwn(rel)
			return err
		}}
	}
	ns := s.opts.Namespaces[i]
	return mailboxbase.Auto{
		Mailboxes: ns.AutoMailboxes(),
		Limit:     s.opts.QuotaPolicy.MailboxCount,
		Created: func(rel string) {
			if s.opts.Locker == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := s.opts.Locker.Emit(ctx, locks.MailboxListKey(personal.Username), locks.EventMailboxCreate, ns.Prefix+rel); err != nil {
				slog.Debug("lmtp: emit list event failed", "folder", ns.Prefix+rel, "err", err)
			}
		},
		Subscribe: func(rel string) error {
			owner := locks.Owner(personal.Username, s.stampLockID(personal).LockID())
			store, key := subs.ForNamespace(s.opts.Namespaces, i, nsUI, personal, owner, s.opts.Locker)
			_, err := store.AddOwn(key + rel)
			return err
		},
	}
}

// personalAuto is the primary personal namespace's, for the recipient's store.
func (s *session) personalAuto(personal *mailbox.UserInfo) mailboxbase.Auto {
	return s.boxAuto(personal, mailbox.PrimaryPersonalIndex(config.NamespaceShapes(s.opts.Namespaces)), personal)
}

// namespaceIndex is ns's place in the configuration.
func (s *session) namespaceIndex(ns *config.NamespaceConfig) int {
	for i := range s.opts.Namespaces {
		if &s.opts.Namespaces[i] == ns {
			return i
		}
	}
	return -1
}

// subscriberFor subscribes a folder a delivery made under lda_mailbox_autocreate,
// in the file a configured mailbox of the same namespace would go to.
func (s *session) subscriberFor(personal *mailbox.UserInfo, folder string) func(rel string) error {
	ns := s.matchNamespace(folder)
	if ns == nil {
		return s.personalAuto(personal).Subscribe
	}
	loc, ok, err := mailbox.ParseLocation(ns.Location, nil)
	if err != nil || !ok {
		return nil
	}
	ui, err := mailbox.NamespaceUserInfo(personal, loc, ns.Separator)
	if err != nil {
		return nil
	}
	return s.boxAuto(personal, s.namespaceIndex(ns), ui).Subscribe
}

// ldaCreate makes a missing folder a delivery names, subscribing it when
// lda_mailbox_autosubscribe asks; a failure leaves the save to report it.
func (s *session) ldaCreate(personal *mailbox.UserInfo, box mailbox.UserMailbox, mbox mailbox.Box, folder, rel string) {
	if err := mailboxcreate.Folder(box, mbox, rel, s.opts.QuotaPolicy.MailboxCount); err != nil {
		slog.Warn("lmtp: lda_mailbox_autocreate failed", "folder", folder, "err", err)
		return
	}
	if !s.opts.Config.LDAMailboxAutosubscribe {
		return
	}
	if sub := s.subscriberFor(personal, folder); sub != nil {
		if err := sub(rel); err != nil {
			slog.Warn("lmtp: lda_mailbox_autosubscribe failed", "folder", folder, "err", err)
		}
	}
}

// defaultMailbox is where a save to a missing folder goes: the recipient's
// detail mailbox when that is another folder and exists, else INBOX.
func (s *session) defaultMailbox(rcptMbox mailbox.Box, detail, missing string) string {
	if detail != "INBOX" && detail != missing {
		if ok, _ := rcptMbox.FolderExists(detail); ok {
			return detail
		}
	}
	return "INBOX"
}
