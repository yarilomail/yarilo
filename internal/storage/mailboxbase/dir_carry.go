package mailboxbase

import "github.com/yarilomail/yarilo/v2/pkg/mailbox"

// dirTimes is the folder's directory times from a store that keeps a stamp of
// them, taken before and right after a write of ours (#2661).
func dirTimes(box mailbox.UserMailbox, folder string) (mailbox.DirTimes, bool) {
	d, ok := mailbox.Driver(box).(mailbox.DirTimer)
	if !ok {
		return mailbox.DirTimes{}, false
	}
	return d.DirTimes(folder), true
}

// carryInTx carries the stamp over the write begun at pre, in tx's commit.
func carryInTx(box mailbox.UserMailbox, folder string, tx any, pre mailbox.DirTimes, ok bool) {
	c, can := tx.(mailbox.DirCarryTx)
	if !ok || !can {
		return
	}
	post, _ := dirTimes(box, folder)
	c.CarryDirTimes(pre, post)
}
