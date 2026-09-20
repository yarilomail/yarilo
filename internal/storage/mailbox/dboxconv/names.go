package dboxconv

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/mboxenc"
)

// AdoptNames brings the folder directories under root/mailboxes to this
// deployment's encoding, which makes the store one-way from the first open
// (#1586). Bottom-up so a renamed parent cannot invalidate a child's path, and
// each rename is fsynced; a name that does not decode is left alone.
func AdoptNames(mailboxesDir string, utf8 bool) (int, error) {
	var dirs []string
	err := filepath.WalkDir(mailboxesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() || p == mailboxesDir {
			return nil
		}
		if filepath.Base(p) == dboxMailsDir {
			// The message directory, not a folder name.
			return fs.SkipDir
		}
		dirs = append(dirs, p)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("dboxconv: walk %s: %w", mailboxesDir, err)
	}
	// Deepest first.
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], string(filepath.Separator)) > strings.Count(dirs[j], string(filepath.Separator))
	})

	renamed := 0
	for _, dir := range dirs {
		parent, name := filepath.Split(dir)
		want, ok := adoptedName(name, utf8)
		if !ok || want == name {
			continue
		}
		done, err := adoptOne(mailboxesDir, filepath.Clean(parent), name, want, utf8)
		if err != nil {
			return renamed, err
		}
		if done {
			renamed++
		}
	}
	return renamed, nil
}

// adoptOne renames one folder, resolving its parent both before the attempt and
// after a miss: a twin pass may move an ancestor at either moment (#1886).
func adoptOne(root, parent, name, want string, utf8 bool) (bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		at, ok := currentDir(root, parent, utf8)
		if !ok {
			return false, nil
		}
		src, target := filepath.Join(at, name), filepath.Join(at, want)
		if _, err := os.Stat(target); err == nil {
			// Source gone: a twin or a crash halfway already renamed. Source
			// still there: two folders would become one, which nothing undoes (#1609).
			if _, serr := os.Stat(src); os.IsNotExist(serr) {
				return false, nil
			}
			return false, fmt.Errorf("dboxconv: renaming %s to %s: the target already exists", src, target)
		}
		// The directory is opened before the rename and synced through that
		// descriptor: a twin moving an ancestor after the rename leaves the
		// path naming nothing, and the rename that succeeded would report
		// ENOENT (#1938).
		dir, derr := os.Open(at)
		if derr != nil {
			return false, fmt.Errorf("dboxconv: open %s: %w", at, derr)
		}
		beforeRename(src)
		err := os.Rename(src, target)
		if err == nil {
			afterRename(src)
			ferr := syncAndClose(dir)
			if ferr != nil {
				return false, ferr
			}
			return true, nil
		}
		_ = dir.Close()
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("dboxconv: rename %s to %s: %w", src, target, err)
		}
		// Gone from under the walk: either already adopted, or an ancestor
		// moved and the same folder is one path over.
		if _, serr := os.Stat(target); serr == nil {
			return false, nil
		}
	}
	return false, nil
}

// beforeRename and afterRename are seams for the twin-pass rows; nil cost in
// production. The two moments differ: one leaves the source somewhere else, the
// other leaves the directory the rename has to be made durable through with no
// path at all (#1938).
var (
	beforeRename = func(string) {}
	afterRename  = func(string) {}
)

// syncAndClose flushes the directory entry through the descriptor the caller
// opened, so a rename inside it survives a crash rather than merely having been
// asked for -- whatever the directory's path has become since.
func syncAndClose(dir *os.File) error {
	serr := dir.Sync()
	cerr := dir.Close()
	if serr != nil {
		return fmt.Errorf("dboxconv: fsync %s: %w", dir.Name(), serr)
	}
	if cerr != nil {
		return fmt.Errorf("dboxconv: fsync %s: %w", dir.Name(), cerr)
	}
	return nil
}

// currentDir follows a path collected before a twin pass may have renamed part
// of it: each segment is either still theirs or already adopted (#1886).
func currentDir(root, dir string, utf8 bool) (string, bool) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", false
	}
	at := root
	if rel == "." {
		return at, true
	}
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if fi, serr := os.Stat(filepath.Join(at, seg)); serr == nil && fi.IsDir() {
			at = filepath.Join(at, seg)
			continue
		}
		want, ok := adoptedName(seg, utf8)
		if !ok {
			return "", false
		}
		if fi, serr := os.Stat(filepath.Join(at, want)); serr != nil || !fi.IsDir() {
			return "", false
		}
		at = filepath.Join(at, want)
	}
	return at, true
}

// adoptedName returns the name this deployment would write for a directory
// currently named name, and whether it could tell.
func adoptedName(name string, utf8 bool) (string, bool) {
	if isASCII(name) && !strings.Contains(name, "&") {
		// The encodings agree on ASCII, except for "&": the modified UTF-7
		// escape, so a name carrying one differs between them.
		return name, true
	}
	if utf8 {
		decoded, err := mboxenc.FromModUTF7(name)
		if err != nil {
			// Not their encoding, so nothing to bring across.
			return name, false
		}
		return decoded, true
	}
	// A name unchanged by decode-and-re-encode is already theirs: re-encoding
	// would escape its "&" into the double encoding this removes.
	if decoded, err := mboxenc.FromModUTF7(name); err == nil && mboxenc.ToModUTF7(decoded) == name {
		return name, true
	}
	return mboxenc.ToModUTF7(name), true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
