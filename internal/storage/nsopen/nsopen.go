// Package nsopen is the one opener of a namespace that is not the user's own
// mail: its storage identity, mail and index handles, subscriptions and ACL.
package nsopen

import (
	"fmt"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/internal/userstate/acl"
	"github.com/yarilomail/yarilo/v2/internal/userstate/subs"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// Namespace is the part of a namespace's configuration that places its store.
type Namespace struct {
	Type      string
	Prefix    string
	Separator string
	Location  string
}

// Fixed is the identity a namespace with a fixed location is opened as for
// user. expand fills the location's variables; nil empties them.
// ok is false for a location that names no store.
func Fixed(ns Namespace, user, expand *mailbox.UserInfo, kind mailbox.StoreKind) (ui *mailbox.UserInfo, ok bool, err error) {
	loc, ok, err := mailbox.ParseLocation(ns.Location, expand)
	if err != nil || !ok {
		return nil, ok, err
	}
	ui, err = mailbox.NamespaceUserInfo(user, loc, ns.Separator, kind)
	if err != nil {
		return nil, true, err
	}
	return ui, true, nil
}

// Kind is the store a namespace keeps: one per user, or one for everybody.
func Kind(ns Namespace) mailbox.StoreKind { return mailbox.StoreKindOf(ns.Type, ns.Prefix) }

// Owner is the identity an owner-templated namespace is opened as for owner,
// base carrying the deployment's storage-name form.
func Owner(ns Namespace, owner, base *mailbox.UserInfo) (*mailbox.UserInfo, error) {
	return mailbox.StampOwnerLocation(owner, base, ns.Location, sepByte(ns.Separator))
}

func sepByte(sep string) byte {
	if sep == "" {
		return 0
	}
	return sep[0]
}

// Backends are the mail and index backends a namespace's store opens on.
type Backends struct {
	Mailbox   mailbox.MailboxBackend
	ByDriver  func(string) mailbox.MailboxBackend
	Overrides map[string]mailbox.MailboxBackend
	Index     mailbox.IndexBackend
}

// For selects the backend of a namespace: its per-prefix override, then the
// driver its identity names, then the global one.
func (b Backends) For(prefix string, ui *mailbox.UserInfo) mailbox.MailboxBackend {
	if o, ok := b.Overrides[prefix]; ok && o != nil {
		return o
	}
	if ui != nil {
		return mailbox.SelectPersonalBackend(b.Mailbox, b.ByDriver, ui.Driver)
	}
	return b.Mailbox
}

// Mode is how much an open settles on disk.
type Mode int

const (
	// Init makes the store's directories.
	Init Mode = iota
	// Deferred makes nothing; a later write materialises what it needs.
	Deferred
)

// Opened is a namespace's store for one user.
type Opened struct {
	Store mailbox.UserMailbox
	Index mailbox.UserIndex
	Box   mailbox.Box
	// AdoptErr is a foreign-name adoption that failed; the store is served anyway.
	AdoptErr error
}

// Open opens ui's store on mb. adoptForeign renames folders another
// implementation left named its own way before anything lists them (#1609).
func Open(mb mailbox.MailboxBackend, idx mailbox.IndexBackend, ui *mailbox.UserInfo, mode Mode, adoptForeign bool, opts ...mailboxbase.BoxOption) (*Opened, error) {
	store := mb.OpenUser(ui)
	if mode == Init {
		if err := store.Init(); err != nil {
			return nil, fmt.Errorf("mailbox init: %w", err)
		}
	}
	o := &Opened{Store: store, Index: idx.OpenUser(ui)}
	if adoptForeign {
		if a, ok := o.Index.(mailbox.ForeignNameAdopter); ok {
			o.AdoptErr = a.AdoptForeignNames()
		}
	}
	o.Box = mailboxbase.Open(store, o.Index, opts...)
	return o, nil
}

// Subs is ui's subscription list in file, under the store's lock name.
func Subs(ui *mailbox.UserInfo, file, owner string, locker locks.Locker) *subs.Store {
	s := subs.New(mailbox.ControlRoot(ui), file, ui.Username, owner, locker)
	s.SetLockName(ui.LockName)
	return s
}

// ACL is the ACL store of ui's namespace rooted at home, read for user.
func ACL(home string, ui *mailbox.UserInfo, user, owner string, pol acl.Policy, locker locks.Locker) *acl.Store {
	s := acl.New(home, ui.MailPath, ui.Driver, ui.Separator, ui.StorageEscapeChar, user, owner, pol, locker)
	s.SetLockName(ui.LockName)
	return s
}
