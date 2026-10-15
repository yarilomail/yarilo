package mailops

import (
	"context"
	"log/slog"
	"time"

	"github.com/yarilomail/yarilo/internal/mailboxcreate"
	"github.com/yarilomail/yarilo/internal/storage/search"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// RenameSeparator joins the old and new names in a rename event's payload.
const RenameSeparator = "\x00"

// FolderACL is a namespace's per-folder ACL state, which follows its folder.
type FolderACL interface {
	Rename(oldFolder, newFolder string) error
	Remove(folder string) error
}

// CreateFolder makes rel in box within quota_mailbox_count, as every creation
// is in the reference, and announces it as name, the caller's full name.
func (o *Ops) CreateFolder(box mailbox.Box, rel, name string) error {
	if err := mailboxcreate.Folder(box, rel, o.cfg.Quota.Policy.MailboxCount); err != nil {
		return err
	}
	o.EmitList(locks.EventMailboxCreate, name)
	return nil
}

// DeleteFolder removes rel from box, its ACL file when acl is set, takes its
// documents out of the index and announces it as name.
func (o *Ops) DeleteFolder(box mailbox.Box, acl FolderACL, rel, name string) error {
	// The identity, read while the folder still has one: after the delete
	// nothing can name its documents (#2022).
	f, ferr := mailbox.Counting(box).Folder(rel, 0)
	if ferr != nil {
		slog.Warn(o.cfg.Protocol+": folder identity before delete", "folder", name, "err", ferr)
	}
	if err := box.Delete(rel); err != nil {
		return err
	}
	if ferr == nil {
		o.ftsDropFolder(f)
	}
	// The folder is gone; a stale ACL file must not fail the answer.
	if acl != nil {
		if err := acl.Remove(rel); err != nil {
			slog.Warn(o.cfg.Protocol+": acl remove after delete failed", "folder", name, "err", err)
		}
	}
	o.EmitList(locks.EventMailboxDelete, name)
	return nil
}

// RenameFolder renames oldRel to newRel in box, moves its ACL file when acl
// is set, and announces the full names.
func (o *Ops) RenameFolder(box mailbox.Box, acl FolderACL, oldRel, newRel, oldName, newName string) error {
	if err := box.Rename(oldRel, newRel); err != nil {
		return err
	}
	if acl != nil {
		if err := acl.Rename(oldRel, newRel); err != nil {
			slog.Warn(o.cfg.Protocol+": acl rename failed", "from", oldName, "to", newName, "err", err)
		}
	}
	o.EmitList(locks.EventMailboxRename, oldName+RenameSeparator+newName)
	return nil
}

// Subscribed announces a subscription change the caller has written.
func (o *Ops) Subscribed(name string, on bool) {
	ev := locks.EventMailboxSubscribe
	if !on {
		ev = locks.EventMailboxUnsubscribe
	}
	o.EmitList(ev, name)
}

// EmitList publishes a change to the user's folder list; best effort.
func (o *Ops) EmitList(ev locks.EventType, payload string) {
	if o.cfg.Locker == nil || o.user == nil || o.user.Username == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := o.cfg.Locker.Emit(ctx, locks.MailboxListKey(o.user.Username), ev, payload); err != nil {
		slog.Debug(o.cfg.Protocol+": emit list event failed", "type", string(ev), "payload", payload, "err", err)
	}
}

// ftsDropFolder retracts a deleted folder off the command path; a rescan's
// orphan sweep makes a lost one harmless (#2022).
func (o *Ops) ftsDropFolder(f *mailbox.Folder) {
	client := o.cfg.FTS
	if client == nil || o.user == nil || f == nil || f.Name == "" {
		return
	}
	user := o.user.Username
	if f.GUID == ([16]byte{}) {
		WarnFolderWithoutGUID(o.cfg.Protocol, user, f.Name)
		return
	}
	mbox := search.RefOf(f)
	go func() {
		if err := client.DropFolder(user, mbox); err != nil {
			slog.Warn(o.cfg.Protocol+": fts drop folder failed", "user", user, "folder", mbox.Name, "err", err)
		}
	}()
}
