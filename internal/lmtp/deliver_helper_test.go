package lmtp

import (
	"io"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailops"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// deliverTest delivers through a layer with no services wired.
func deliverTest(box mailbox.Box, folder string, r io.ReadSeeker, size int64, username, from string, flags []string) (uint32, mailbox.Folder, [16]byte, error) {
	ops := mailops.NewDelivery(mailops.Config{Protocol: "lmtp"}, box, &mailbox.UserInfo{Username: username})
	return deliverOne(ops, box, folder, r, size, username, from, flags)
}
