package mailboxbase

import (
	"github.com/yarilomail/yarilo/internal/mailboxcreate"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Auto is what a namespace makes for its user on any open, whichever server
// opens it, as the reference's storage library does; hooks are the caller's.
type Auto struct {
	Mailboxes map[string]mailbox.AutoMailbox
	Limit     int64
	// Created runs after a folder is made: the caller's ACL and list event.
	Created func(rel string)
	// Subscribe records a made folder as subscribed; nil keeps none.
	Subscribe func(rel string) error
}

// WithAuto makes the configured mailboxes on any open, list or existence check.
func WithAuto(a Auto) BoxOption {
	return func(b *Box) {
		if len(a.Mailboxes) > 0 {
			b.auto = &a
		}
	}
}

func (b *Box) autoTarget() mailboxcreate.Target {
	return mailboxcreate.Target{Store: b.store, Box: b, Limit: b.auto.Limit,
		Created: b.auto.Created, Subscribe: b.auto.Subscribe}
}

// makesFolders is false for a diagnostic, which reads an account as it is.
func (b *Box) makesFolders() bool { return b.auto != nil && b.mode != openReadOnly }

func (b *Box) makeConfigured(name string) {
	if b.makesFolders() {
		mailboxcreate.One(b.autoTarget(), b.auto.Mailboxes, name)
	}
}

// ListFolders lists the store's folders, the configured ones made first.
func (b *Box) ListFolders() ([]mailbox.FolderEntry, error) {
	entries, err := b.store.ListFolders()
	if err != nil || !b.makesFolders() {
		return entries, err
	}
	if len(mailboxcreate.Ensure(b.autoTarget(), b.auto.Mailboxes, entries)) == 0 {
		return entries, nil
	}
	return b.store.ListFolders()
}

// FolderExists answers for the store, making the folder first when configured.
func (b *Box) FolderExists(name string) (bool, error) {
	b.makeConfigured(name)
	return b.store.FolderExists(name)
}
