package virtual

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ErrNotStored says the operation asks a virtual mailbox to hold a message.
// It holds none: a message lives in the folder it was delivered to, and the
// virtual mailbox names it.
var ErrNotStored = errors.New("virtual: a virtual mailbox stores no message of its own")

// ErrUnsupported says the namespace does not offer the operation at all.
var ErrUnsupported = errors.New("virtual: not supported in a virtual namespace")

// Backend serves a namespace whose location is "virtual:<path>". Every
// subdirectory of that path holding a configuration file is one mailbox.
type Backend struct{}

// New returns the backend. It keeps no state: a handle is per user.
func New() *Backend { return &Backend{} }

func (b *Backend) OpenUser(info *mailbox.UserInfo) mailbox.UserMailbox {
	return &userMailbox{info: info, root: info.MailPath}
}

type userMailbox struct {
	info *mailbox.UserInfo
	root string
}

func (u *userMailbox) Username() string { return u.info.Username }

// Init makes the namespace directory. The mailboxes inside it are made by an
// operator, as the reference has it: a configuration file is what creates one.
func (u *userMailbox) Init() error {
	if u.root == "" {
		return errors.New("virtual: the namespace has no path")
	}
	if err := os.MkdirAll(u.root, 0o700); err != nil {
		return fmt.Errorf("virtual: create %s: %w", u.root, err)
	}
	return nil
}

// Create refuses: a virtual mailbox is defined by its configuration file, so
// making one from a client would leave a mailbox with no definition.
func (u *userMailbox) Create(string) error {
	return fmt.Errorf("virtual: a mailbox is made by its configuration file: %w", ErrUnsupported)
}

func (u *userMailbox) Delete(string) error { return ErrUnsupported }

func (u *userMailbox) Rename(string, string) error { return ErrUnsupported }

func (u *userMailbox) Save(string, io.Reader, uint32, int64, []string, []string, [16]byte) (string, uint32, [16]byte, error) {
	return "", 0, [16]byte{}, ErrNotStored
}

func (u *userMailbox) Move(string, string, string, [16]byte) (string, [16]byte, error) {
	return "", [16]byte{}, ErrNotStored
}

func (u *userMailbox) Fetch(string, string, bool) (io.ReadCloser, error) {
	// The backing folder holds the message; reading it through this handle is
	// the next step's work.
	return nil, ErrNotStored
}

func (u *userMailbox) Remove(string, string) error { return ErrNotStored }

// List answers with nothing: what a virtual mailbox holds is in its index,
// not in a directory of its own.
func (u *userMailbox) List(string) ([]*mailbox.MessageMeta, error) { return nil, nil }

// Scan likewise: there is no storage to scan, so a rebuild from storage would
// empty the mailbox rather than repair it.
func (u *userMailbox) Scan(string) ([]mailbox.ScanRecord, error) { return nil, ErrUnsupported }

func (u *userMailbox) FolderExists(folder string) (bool, error) {
	if folder == "" {
		return false, nil
	}
	_, err := LoadConfig(u.folderDir(folder))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNoConfig):
		return false, nil
	default:
		return false, err
	}
}

// ListFolders names every directory under the namespace that holds a
// configuration file, at any depth: a directory without one is not a mailbox.
func (u *userMailbox) ListFolders() ([]mailbox.FolderEntry, error) {
	var out []mailbox.FolderEntry
	err := filepath.WalkDir(u.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == u.root {
			return nil //nolint:nilerr // an unreadable subtree is not a mailbox
		}
		if !hasConfig(path) {
			return nil
		}
		rel, rerr := filepath.Rel(u.root, path)
		if rerr != nil {
			return nil
		}
		out = append(out, mailbox.FolderEntry{Name: u.nameOf(rel)})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("virtual: read %s: %w", u.root, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// hasConfig says whether a directory defines a mailbox.
func hasConfig(dir string) bool {
	for _, name := range []string{ConfigFileName, LegacyConfigFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// The namespace's hierarchy is directories: the client's separator is the
// path separator on disk, and nothing else is rewritten.
func (u *userMailbox) nameOf(rel string) string {
	return strings.ReplaceAll(rel, "/", mailbox.SepOrDefault(u.info.Separator))
}

func (u *userMailbox) Close() error { return nil }

// Config reads one virtual mailbox's definition.
func (u *userMailbox) Config(folder string) (*Config, error) { return LoadConfig(u.folderDir(folder)) }

func (u *userMailbox) folderDir(folder string) string {
	return filepath.Join(u.root, strings.ReplaceAll(folder, mailbox.SepOrDefault(u.info.Separator), "/"))
}
