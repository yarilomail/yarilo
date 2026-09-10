package dboxv2

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// brokenSuffix names a body set aside beside its own file, as the reference
// does and as mdbox already does: no directory of our own (#1718).
const brokenSuffix = ".broken"

// guidNamedFile is a message still under the name a guid gave it.
type guidNamedFile struct {
	name string
	guid [16]byte
	age  time.Duration
}

// guidNamedFiles lists them with their age, which is what tells a save still in
// flight from one a crash left behind (#1718).
func guidNamedFiles(dir string) ([]guidNamedFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("sdbox/orphan: list %s: %w", dir, err)
	}
	var out []guidNamedFile
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), sdboxMailPrefix)
		if !ok || len(rest) != 32 {
			continue
		}
		raw, herr := hex.DecodeString(rest)
		if herr != nil {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		f := guidNamedFile{name: e.Name(), age: time.Since(info.ModTime())}
		copy(f.guid[:], raw)
		out = append(out, f)
	}
	return out, nil
}

// adoptOrphans places what the rename pass could not, and returns what is still
// there afterwards -- which is what holds the marker (#1718).
func (u *userMailbox) adoptOrphans(idx mailbox.UserIndex, folder *mailbox.Folder, msgs []*mailbox.MessageMeta) (adopted, left int, err error) {
	dir := u.folderPath(folder.Name)
	files, err := guidNamedFiles(dir)
	if err != nil {
		return 0, 0, err
	}
	held := make(map[[16]byte]uint32, len(msgs))
	for _, m := range msgs {
		held[m.GUID] = m.UID
	}
	for _, f := range files {
		if f.age < staleTemp {
			// A save in flight: its own caller is about to name it, and taking
			// it now would take a message from the session storing it.
			left++
			continue
		}
		if uid, dup := held[f.guid]; dup {
			// Not an orphan: the record holding this guid has its own file, so
			// filing this one would make two messages of one (#1718).
			if serr := u.setAside(folder.Name, f.name,
				fmt.Sprintf("uid %d already holds this guid", uid), nil); serr != nil {
				return adopted, left + 1, serr
			}
			continue
		}
		switch placed, perr := u.refileOrphan(idx, folder, f); {
		case perr != nil:
			return adopted, left + 1, perr
		case placed:
			adopted++
		default:
			// Set aside: out of the folder, so it holds nothing back.
		}
	}
	return adopted, left, nil
}

// refileOrphan gives the body a record of its own, keeping the guid its name
// carries so the message keeps the id a client may already hold.
func (u *userMailbox) refileOrphan(idx mailbox.UserIndex, folder *mailbox.Folder, f guidNamedFile) (bool, error) {
	path := filepath.Join(u.folderPath(folder.Name), f.name)
	rc, oerr := u.Fetch(folder.Name, f.name, false)
	if oerr != nil {
		return false, u.setAside(folder.Name, f.name, "no reader accepts it", oerr)
	}
	raw, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		return false, u.setAside(folder.Name, f.name, "no reader accepts it", rerr)
	}
	saved, vsize, guid, serr := u.Save(folder.Name, bytes.NewReader(raw), 0, int64(len(raw)), nil, f.guid)
	if serr != nil {
		return false, fmt.Errorf("sdbox/orphan: save %s: %w", f.name, serr)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(raw)), VSize: vsize, GUID: guid}
	if aerr := mailboxbase.RecordSaved(idx, u, folder.ID, folder.Name, saved, m); aerr != nil {
		return false, fmt.Errorf("sdbox/orphan: record %s: %w", f.name, aerr)
	}
	if derr := os.Remove(path); derr != nil && !os.IsNotExist(derr) {
		return false, fmt.Errorf("sdbox/orphan: remove %s: %w", f.name, derr)
	}
	slog.Info("sdbox: a body no record named was filed as a message of its own",
		"user", u.username, "folder", folder.Name, "was", f.name, "uid", m.UID)
	return true, nil
}

// setAside renames a body out of the pass's way, beside itself. Never deleted:
// it is the only copy, and what it is worth is the operator's to judge.
func (u *userMailbox) setAside(folder, name, why string, cause error) error {
	from := filepath.Join(u.folderPath(folder), name)
	if err := os.Rename(from, from+brokenSuffix); err != nil {
		return fmt.Errorf("sdbox/orphan: set aside %s: %w", name, err)
	}
	slog.Error("sdbox: a body the folder cannot place was set aside",
		"user", u.username, "folder", folder, "file", name+brokenSuffix,
		"why", why, "err", cause)
	return nil
}
