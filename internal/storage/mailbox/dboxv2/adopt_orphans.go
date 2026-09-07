package dboxv2

import (
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// brokenDir is where a body no reader can parse is set aside: out of the
// folder, never deleted, and named so an operator can find it.
const brokenDir = ".broken"

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

// adoptOrphans places what the rename pass could not: a file no record names.
// Old enough to be a crash's leftover, it is re-filed as a message of its own
// where the body reads, and set aside where it does not (#1718).
//
// Returns how many are still there afterwards, which is what holds the marker.
func (u *userMailbox) adoptOrphans(idx mailbox.UserIndex, folder *mailbox.Folder) (adopted, left int, err error) {
	dir := u.folderPath(folder.Name)
	files, err := guidNamedFiles(dir)
	if err != nil {
		return 0, 0, err
	}
	for _, f := range files {
		if f.age < staleTemp {
			// A save in flight: its own caller is about to name it, and taking
			// it now would take a message from the session storing it.
			left++
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
	body, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("sdbox/orphan: open %s: %w", f.name, err)
	}
	defer body.Close() //nolint:errcheck
	rc, oerr := u.Fetch(folder.Name, f.name, false)
	if oerr != nil {
		return false, u.setAside(folder.Name, f.name, oerr)
	}
	raw, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		return false, u.setAside(folder.Name, f.name, rerr)
	}
	saved, vsize, guid, serr := u.Save(folder.Name, strings.NewReader(string(raw)), 0, int64(len(raw)), nil, f.guid)
	if serr != nil {
		return false, fmt.Errorf("sdbox/orphan: save %s: %w", f.name, serr)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(raw)), VSize: vsize, GUID: guid}
	if aerr := mailbox.RecordSaved(idx, u, folder.ID, folder.Name, saved, m); aerr != nil {
		return false, fmt.Errorf("sdbox/orphan: record %s: %w", f.name, aerr)
	}
	if derr := os.Remove(path); derr != nil && !os.IsNotExist(derr) {
		return false, fmt.Errorf("sdbox/orphan: remove %s: %w", f.name, derr)
	}
	slog.Info("sdbox: a body no record named was filed as a message of its own",
		"user", u.username, "folder", folder.Name, "was", f.name, "uid", m.UID)
	return true, nil
}

// setAside moves a body nothing can read out of the folder. Never deleted: it
// is the only copy, and what it is worth is the operator's to judge.
func (u *userMailbox) setAside(folder, name string, cause error) error {
	dst := filepath.Join(u.folderPath(folder), brokenDir)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("sdbox/orphan: mkdir %s: %w", dst, err)
	}
	from := filepath.Join(u.folderPath(folder), name)
	if err := os.Rename(from, filepath.Join(dst, name)); err != nil {
		return fmt.Errorf("sdbox/orphan: set aside %s: %w", name, err)
	}
	slog.Error("sdbox: a body no record names and no reader accepts was set aside",
		"user", u.username, "folder", folder, "file", name, "dir", brokenDir, "err", cause)
	return nil
}
