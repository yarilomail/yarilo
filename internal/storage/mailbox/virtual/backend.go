package virtual

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ErrNotStored says the caller asks a virtual mailbox to hold a message: it
// holds none, and names messages that live in other folders.
var ErrNotStored = errors.New("virtual: a virtual mailbox stores no message of its own")

// ErrUnsupported says the namespace does not offer the operation at all.
var ErrUnsupported = errors.New("virtual: not supported in a virtual namespace")

// Backend serves a namespace whose location is "virtual:<path>". Every
// subdirectory of that path holding a configuration file is one mailbox.
type Backend struct{ opts Options }

// New returns the backend. It keeps no state: a handle is per user.
func New(opts ...Options) *Backend {
	b := &Backend{}
	if len(opts) > 0 {
		b.opts = opts[0]
	}
	return b
}

func (b *Backend) OpenUser(info *mailbox.UserInfo) mailbox.UserMailbox {
	return &userMailbox{info: info, root: info.MailPath, opts: b.opts}
}

type userMailbox struct {
	info *mailbox.UserInfo
	root string
	opts Options

	// personalBox is opened once, on the first pass, and closed with this
	// handle: a poll pass runs on every NOOP (#1805).
	personalMu  sync.Mutex
	personalBox mailbox.Box
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
// configuration file, at any depth, following a symlink to a directory.
func (u *userMailbox) ListFolders() ([]mailbox.FolderEntry, error) {
	var out []mailbox.FolderEntry
	seen := map[string]bool{}
	if root, err := filepath.EvalSymlinks(u.root); err == nil {
		seen[root] = true
	}
	// A root that cannot be read is an empty namespace, not a failed LIST:
	// one wrong mode on the definitions breaks listing for every user.
	if err := u.walk(u.root, "", seen, &out); err != nil &&
		!errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) {
		return nil, fmt.Errorf("virtual: read %s: %w", u.root, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// walk descends one directory, resolving each entry rather than trusting its
// type: a mounted definition reaches the namespace as a symlink.
func (u *userMailbox) walk(dir, rel string, seen map[string]bool, out *[]mailbox.FolderEntry) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if rel == "" {
			return err
		}
		return nil //nolint:nilerr // an unreadable subtree is not a mailbox
	}
	for _, e := range entries {
		// A name opening with a dot is the mount's own bookkeeping, never a
		// mailbox: the definitions arrive beside ..data and ..<timestamp>.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		// Resolved, not trusted: a link to a directory is a mailbox, a link
		// to nothing is skipped rather than failing the whole listing.
		info, serr := os.Stat(path)
		if serr != nil || !info.IsDir() {
			continue
		}
		real, rerr := filepath.EvalSymlinks(path)
		if rerr != nil || seen[real] {
			continue
		}
		seen[real] = true
		child := filepath.Join(rel, e.Name())
		if hasConfig(path) {
			// Selectable: a directory with a configuration is a mailbox, and
			// LIST marks the rest \Noselect.
			*out = append(*out, mailbox.FolderEntry{Name: u.nameOf(child), Selectable: true})
		}
		if werr := u.walk(path, child, seen, out); werr != nil {
			return werr
		}
	}
	return nil
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

func (u *userMailbox) Close() error {
	u.personalMu.Lock()
	defer u.personalMu.Unlock()
	if u.personalBox != nil {
		u.personalBox.Close()
		u.personalBox = nil
	}
	return nil
}

// Config reads one virtual mailbox's definition.
func (u *userMailbox) Config(folder string) (*Config, error) { return LoadConfig(u.folderDir(folder)) }

func (u *userMailbox) folderDir(folder string) string {
	return filepath.Join(u.root, strings.ReplaceAll(folder, mailbox.SepOrDefault(u.info.Separator), "/"))
}
