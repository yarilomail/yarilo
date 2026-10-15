package mailops

import (
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ExpungeMarked removes msgs from folder of box under one hold, then runs the hooks
// for each removed message outside it, in the order the hold removed them (#1853).
func (o *Ops) ExpungeMarked(box mailbox.Box, f *mailbox.Folder, folder string, msgs []*mailbox.MessageMeta) ([]*mailbox.MessageMeta, int, error) {
	o.Quota.CaptureSnap()
	removed, failed, err := box.ExpungeMarked(f, folder, msgs)
	for _, m := range removed {
		o.Committed(f, locks.EventExpunged, m.UID, UsageDelta(m), m.GUID)
	}
	return removed, failed, err
}
