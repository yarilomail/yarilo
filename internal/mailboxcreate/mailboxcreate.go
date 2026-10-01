// Package mailboxcreate makes a folder for a protocol server, and the folders a
// namespace's configuration says every user has (#2005).
package mailboxcreate

import (
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ErrLimit is quota_mailbox_count reached.
var ErrLimit = errors.New("mailboxcreate: the mailbox count limit is reached")

// Folder makes one folder in the store and its index; limit is
// quota_mailbox_count, zero for none.
func Folder(store mailbox.UserMailbox, box mailbox.Box, rel string, limit int64) error {
	if limit > 0 {
		if entries, err := store.ListFolders(); err == nil && int64(len(entries)) >= limit {
			return ErrLimit
		}
	}
	if err := store.Create(rel); err != nil {
		return err
	}
	box.CreateFolder(rel, uint32(time.Now().Unix()))
	return nil
}

// Target is one namespace's store and what its caller adds to a creation.
type Target struct {
	Store mailbox.UserMailbox
	Box   mailbox.Box
	Limit int64
	// Created runs after a folder is made: the caller's ACL and event.
	Created func(rel string)
	// Subscribe records a made folder as subscribed; nil keeps none.
	Subscribe func(rel string) error
}

// Ensure makes every configured mailbox the namespace lacks, subscribing the
// ones marked subscribe, and answers the names it made.
func Ensure(t Target, boxes map[string]mailbox.AutoMailbox, existing []mailbox.FolderEntry) []string {
	if len(boxes) == 0 {
		return nil
	}
	have := make(map[string]bool, len(existing))
	for _, e := range existing {
		have[e.Name] = true
	}
	var made []string
	for _, rel := range autoNames(boxes) {
		if have[rel] {
			continue
		}
		if createOne(t, rel, boxes[rel]) {
			made = append(made, rel)
		}
	}
	return made
}

// One makes rel when it is configured and absent, for a SELECT or STATUS that
// names it before any LIST did.
func One(t Target, boxes map[string]mailbox.AutoMailbox, rel string) bool {
	mb, ok := boxes[rel]
	if !ok || mb.Auto == mailbox.AutoNo || mb.Auto == "" {
		return false
	}
	if exists, err := t.Store.FolderExists(rel); err != nil || exists {
		return false
	}
	return createOne(t, rel, mb)
}

func createOne(t Target, rel string, mb mailbox.AutoMailbox) bool {
	if err := Folder(t.Store, t.Box, rel, t.Limit); err != nil {
		slog.Warn("mailboxcreate: configured mailbox not created", "user", t.Store.Username(), "folder", rel, "err", err)
		return false
	}
	if t.Created != nil {
		t.Created(rel)
	}
	if mb.Auto == mailbox.AutoSubscribe && t.Subscribe != nil {
		if err := t.Subscribe(rel); err != nil {
			slog.Warn("mailboxcreate: configured mailbox not subscribed", "user", t.Store.Username(), "folder", rel, "err", err)
		}
	}
	return true
}

func autoNames(boxes map[string]mailbox.AutoMailbox) []string {
	out := make([]string, 0, len(boxes))
	for name, mb := range boxes {
		if mb.Auto == mailbox.AutoCreate || mb.Auto == mailbox.AutoSubscribe {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
