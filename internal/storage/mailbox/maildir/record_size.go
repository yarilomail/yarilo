package maildir

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// RecordSize answers from the name, as the reference does: S= is the physical
// size and W= the virtual one. A name carrying neither is measured (#1726).
func (u *userMailbox) RecordSize(folder string, m *mailbox.MessageMeta) (uint32, uint32, error) {
	name, err := u.RecordPath(folder, m)
	if err != nil {
		return m.Size, m.VSize, err
	}
	if size, vsize, ok := sizesFromName(name); ok {
		return size, vsize, nil
	}
	return measureSizes(filepath.Join(u.folderPath(folder), "cur", name))
}

// sizesFromName reads the ,S= and ,W= fields of a base name. Both or neither:
// a name carrying one number alone says nothing about the other.
func sizesFromName(name string) (size, vsize uint32, ok bool) {
	base := maildirBase(name)
	size, okS := sizeField(base, ",S=")
	vsize, okW := sizeField(base, ",W=")
	if !okS || !okW {
		return 0, 0, false
	}
	return size, vsize, true
}

func sizeField(base, key string) (uint32, bool) {
	i := strings.Index(base, key)
	if i < 0 {
		return 0, false
	}
	rest := base[i+len(key):]
	if j := strings.IndexByte(rest, ','); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.ParseUint(rest, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

var _ mailbox.RecordSizer = (*userMailbox)(nil)
