package maildir

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// dotlockSuffix names the lock file beside the list. Ours is the lock service,
// but a foreign process watches for this one and nothing else.
const dotlockSuffix = ".lock"

// uidRecord is one list entry. Sizes are carried only when the name does not
// already spell them, which is the rule the reference writes by.
type uidRecord struct {
	uid      uint32
	base     string
	guid     [16]byte
	hasGUID  bool
	psize    uint32
	vsize    uint32
	hasSizes bool
}

// uidList is a folder's list: the header the index adopts, and the records in
// the order they were written.
type uidList struct {
	uidValidity uint32
	nextUID     uint32
	guid        string
	records     []uidRecord
	torn        bool // a line no rule explains: what follows it is unrecoverable
}

// readUIDListFile parses the whole file. A line that parses adds a record; the
// first that does not ends the list and marks it torn.
func readUIDListFile(path string) (*uidList, error) {
	listParses.Add(1)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	l := &uidList{}
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if strings.HasPrefix(line, "3 ") {
				parseUIDListHeader(line, l)
				continue
			}
			l.torn = true
			break
		}
		rec, ok := parseUIDListRecord(line)
		if !ok {
			l.torn = true
			break
		}
		l.records = append(l.records, rec)
	}
	if err := sc.Err(); err != nil {
		// A line past the scanner's limit is torn in the same sense: what
		// follows it cannot be reached.
		l.torn = true
	}
	return l, nil
}

func parseUIDListHeader(line string, l *uidList) {
	for _, fld := range strings.Fields(line)[1:] {
		if len(fld) < 2 {
			continue
		}
		switch fld[0] {
		case 'V':
			if n, err := strconv.ParseUint(fld[1:], 10, 32); err == nil {
				l.uidValidity = uint32(n)
			}
		case 'N':
			if n, err := strconv.ParseUint(fld[1:], 10, 32); err == nil {
				l.nextUID = uint32(n)
			}
		case 'G':
			l.guid = fld[1:]
		}
	}
}

func parseUIDListRecord(line string) (uidRecord, bool) {
	sep := strings.Index(line, " :")
	if sep < 0 {
		return uidRecord{}, false
	}
	rec := uidRecord{base: line[sep+2:]}
	fields := strings.Fields(line[:sep])
	if len(fields) == 0 {
		return uidRecord{}, false
	}
	uid, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		return uidRecord{}, false
	}
	rec.uid = uint32(uid)
	for _, fld := range fields[1:] {
		if len(fld) < 2 {
			continue
		}
		switch fld[0] {
		case 'G':
			raw, derr := hex.DecodeString(fld[1:])
			if derr != nil || len(raw) != 16 {
				continue
			}
			copy(rec.guid[:], raw)
			rec.hasGUID = true
		case 'S':
			if n, perr := strconv.ParseUint(fld[1:], 10, 32); perr == nil {
				rec.psize, rec.hasSizes = uint32(n), true
			}
		case 'W':
			if n, perr := strconv.ParseUint(fld[1:], 10, 32); perr == nil {
				rec.vsize, rec.hasSizes = uint32(n), true
			}
		}
	}
	return rec, true
}

// String renders one record: keys before the separator, base name after it.
func (r uidRecord) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d", r.uid)
	if r.hasGUID {
		fmt.Fprintf(&b, " G%s", hex.EncodeToString(r.guid[:]))
	}
	if r.hasSizes {
		fmt.Fprintf(&b, " S%d W%d", r.psize, r.vsize)
	}
	fmt.Fprintf(&b, " :%s", r.base)
	return b.String()
}

// nameCarriesSizes reports whether the base already spells both numbers, in
// which case a record repeating them is the reference's "omit the keys" case.
func nameCarriesSizes(base string) bool {
	return strings.Contains(base, ",S=") && strings.Contains(base, ",W=")
}

// measureSizes reads the file for the two numbers a name does not carry: the
// physical size, and the virtual one a lone LF grows under CRLF.
func measureSizes(path string) (psize, vsize uint32, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close() //nolint:errcheck
	c := &sizeCounter{}
	if _, err := io.Copy(c, f); err != nil {
		return 0, 0, err
	}
	return c.phys, c.phys + c.lfNoCR, nil
}

// writeUIDList rewrites the whole file: a temp, synced, then renamed under the
// dotlock. The header's next uid is recomputed, so none is handed out twice.
func (u *userMailbox) writeUIDList(folder string, l *uidList) error {
	path := u.uidListPath(folder)
	unlock, err := u.dotlock(path)
	if err != nil {
		return err
	}
	defer unlock()

	next := l.nextUID
	for _, rec := range l.records {
		if rec.uid >= next {
			next = rec.uid + 1
		}
	}
	if next == 0 {
		next = 1
	}
	l.nextUID = next
	if l.uidValidity == 0 {
		l.uidValidity = uint32(time.Now().Unix())
	}
	if l.guid == "" {
		l.guid = randomGUID()
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("maildir/uidlist: create tmp: %w", err)
	}
	bw := bufio.NewWriter(f)
	fmt.Fprintf(bw, "3 V%d N%d G%s\n", l.uidValidity, l.nextUID, l.guid) //nolint:errcheck
	for _, rec := range l.records {
		fmt.Fprintln(bw, rec.String()) //nolint:errcheck
	}
	if err := bw.Flush(); err != nil {
		f.Close()      //nolint:errcheck
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("maildir/uidlist: write: %w", err)
	}
	// Synced before the rename: a crash in between otherwise leaves a list of
	// zero length, and every file behind it takes a fresh uid.
	if err := syncFile(f); err != nil {
		f.Close()      //nolint:errcheck
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("maildir/uidlist: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("maildir/uidlist: close: %w", err)
	}
	if beforeUIDListRename != nil {
		beforeUIDListRename(tmp)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("maildir/uidlist: rename: %w", err)
	}
	return nil
}

// syncFile is the durability call, and beforeUIDListRename runs after it. Test
// seams: the order is measured, not read off the comment above it.
var (
	syncFile            = (*os.File).Sync
	beforeUIDListRename func(tmp string)
)

// dotlock takes the lock file beside the list with O_EXCL and returns its
// release. A stale one older than staleDotlock is removed, not waited on.
func (u *userMailbox) dotlock(path string) (func(), error) {
	lock := path + dotlockSuffix
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			f.Close()                              //nolint:errcheck
			return func() { os.Remove(lock) }, nil //nolint:errcheck
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("maildir/uidlist: dotlock: %w", err)
		}
		st, serr := os.Stat(lock)
		if serr == nil && time.Since(st.ModTime()) > staleDotlock {
			slog.Warn("maildir: removing a stale uidlist lock",
				"file", lock, "age_s", int(time.Since(st.ModTime()).Seconds()))
			os.Remove(lock) //nolint:errcheck
			continue
		}
		if attempt >= dotlockAttempts {
			return nil, fmt.Errorf("maildir/uidlist: %s held by another process", lock)
		}
		time.Sleep(dotlockRetry)
	}
}

const (
	// staleDotlock is when a lock file stops meaning "someone is writing" and
	// starts meaning "someone died".
	staleDotlock = 2 * time.Minute
	// dotlockAttempts x dotlockRetry bounds the wait for a live writer.
	dotlockAttempts = 100
	dotlockRetry    = 20 * time.Millisecond
)

// listEntry is one uid the caller has just assigned to a name.
type listEntry struct {
	uid      uint32
	filename string
}

// recordUIDsLocked writes a whole batch of assignments in one rewrite: per
// message the list would be rewritten once for every message it already holds.
// recordUIDsLocked writes a batch of rows in one rewrite and returns the uids
// it refused: a base already listed under another uid keeps its owner (#1745).
func (u *userMailbox) recordUIDsLocked(folder string, entries []listEntry) ([]uint32, error) {
	if err := u.ensureUIDListLocked(folder); err != nil {
		return nil, err
	}
	path := u.uidListPath(folder)
	l, err := readUIDListFile(path)
	if err != nil {
		return nil, fmt.Errorf("maildir/uidlist: read: %w", err)
	}
	if l.torn {
		u.reportTornUIDList(folder, path, l)
	}
	beforeRows, beforeMod, beforeSize := len(l.records), int64(0), int64(0)
	if listDebug() {
		beforeMod, beforeSize = u.listStat(folder)
	}
	at := make(map[string]int, len(l.records))
	for i, rec := range l.records {
		at[rec.base] = i
	}
	var taken []uint32
	for _, e := range entries {
		base := maildirBase(e.filename)
		if i, ok := at[base]; ok && l.records[i].uid != e.uid {
			taken = append(taken, e.uid)
			continue
		}
		rec := uidRecord{uid: e.uid, base: base}
		if !nameCarriesSizes(base) {
			psize, vsize, merr := measureSizes(filepath.Join(u.folderPath(folder), "cur", e.filename))
			if merr == nil {
				rec.psize, rec.vsize, rec.hasSizes = psize, vsize, true
			}
		}
		if i, ok := at[base]; ok {
			l.records[i] = rec
			continue
		}
		at[base] = len(l.records)
		l.records = append(l.records, rec)
	}
	if err := u.writeUIDList(folder, l); err != nil {
		return nil, err
	}
	uids := make([]uint32, 0, len(entries))
	for _, e := range entries {
		uids = append(uids, e.uid)
	}
	u.debugListWrite("reconcile-import", folder, uids, "", beforeRows, beforeMod, beforeSize)
	u.folderCacheFor(folder).invalidateUIDs()
	return taken, nil
}

// ensureUIDListLocked gives a folder with none a header-only list: the index
// can adopt its UIDVALIDITY only while the folder is still empty (#1701).
func (u *userMailbox) ensureUIDListLocked(folder string) error {
	if err := u.migrateLegacyUIDList(folder); err != nil {
		return err
	}
	path := u.uidListPath(folder)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("maildir/uidlist: stat: %w", err)
	}
	if err := os.MkdirAll(u.controlFolderPath(folder), 0o700); err != nil {
		return fmt.Errorf("maildir/uidlist: mkdir control: %w", err)
	}
	return u.writeUIDList(folder, &uidList{})
}

// SetTestWriteSeams points the durability call and the pre-rename hook at a
// test's own. Both nil restores the real ones.
func SetTestWriteSeams(sync func(*os.File) error, beforeRename func(string)) {
	if sync == nil {
		sync = (*os.File).Sync
	}
	syncFile, beforeUIDListRename = sync, beforeRename
}

// placeUIDsLocked writes a batch in one rewrite of the list, and returns the
// uids it refused: a base listed under another uid stays with its owner (#1726).
func (u *userMailbox) placeUIDsLocked(folder string, place map[uint32]string) (int, []uint32, error) {
	if err := u.ensureUIDListLocked(folder); err != nil {
		return 0, nil, err
	}
	path := u.uidListPath(folder)
	l, err := readUIDListFile(path)
	if err != nil {
		return 0, nil, fmt.Errorf("maildir/uidlist: read: %w", err)
	}
	if l.torn {
		u.reportTornUIDList(folder, path, l)
	}
	beforeRows, beforeMod, beforeSize := len(l.records), int64(0), int64(0)
	if listDebug() {
		beforeMod, beforeSize = u.listStat(folder)
	}
	listed := make(map[string]int, len(l.records))
	for i := range l.records {
		listed[l.records[i].base] = i
	}
	placed := 0
	var taken []uint32
	for uid, base := range place {
		if uid == 0 {
			continue
		}
		if i, ok := listed[base]; ok && l.records[i].uid != uid {
			taken = append(taken, uid)
			continue
		}
		if testPlaceLimit > 0 && placed >= testPlaceLimit {
			break
		}
		rec := uidRecord{uid: uid, base: base}
		if !nameCarriesSizes(base) {
			if psize, vsize, merr := measureSizes(filepath.Join(u.folderPath(folder), "cur", base)); merr == nil {
				rec.psize, rec.vsize, rec.hasSizes = psize, vsize, true
			}
		}
		if i, ok := listed[base]; ok {
			l.records[i] = rec
		} else {
			l.records = append(l.records, rec)
		}
		placed++
	}
	if placed == 0 {
		return 0, taken, nil
	}
	if err := u.writeUIDList(folder, l); err != nil {
		return 0, taken, err
	}
	placedUIDs := make([]uint32, 0, len(place))
	for uid := range place {
		placedUIDs = append(placedUIDs, uid)
	}
	u.debugListWrite("migrate-place", folder, placedUIDs, "", beforeRows, beforeMod, beforeSize)
	return placed, taken, nil
}

// testPlaceLimit caps how many entries one pass writes. Test seam: a partial
// placement is the state the sidecar must survive, and nothing else makes one.
var testPlaceLimit int

// SetTestPlaceLimit caps the batch and returns a function restoring it.
func SetTestPlaceLimit(n int) func() {
	testPlaceLimit = n
	return func() { testPlaceLimit = 0 }
}

// listDebug gates every reading the rows need: the "before" side once cost a
// second parse of the whole list on every save (#1739).
func listDebug() bool {
	return slog.Default().Enabled(context.Background(), slog.LevelDebug)
}

// listStat is the file's identity for a debug row, without reading it.
func (u *userMailbox) listStat(folder string) (mtime, size int64) {
	fi, err := os.Stat(u.uidListPath(folder))
	if err != nil {
		return 0, 0
	}
	return fi.ModTime().UnixNano(), fi.Size()
}

// listRows counts the file, for the "after" side alone: the "before" side is
// the record set the writer already holds.
func (u *userMailbox) listRows(folder string) int {
	l, err := readUIDListFile(u.uidListPath(folder))
	if err != nil {
		return -1
	}
	return len(l.records)
}

// debugListWrite names who wrote what, and what the file looked like on both
// sides of the write. DEBUG, so it costs nothing until an operator asks.
func (u *userMailbox) debugListWrite(site, folder string, uids []uint32, base string, beforeRows int, beforeMod, beforeSize int64) {
	if !listDebug() {
		return
	}
	rows := u.listRows(folder)
	mod, size := u.listStat(folder)
	slog.Debug("maildir: uidlist written",
		"site", site, "user", u.username, "folder", folder, "owner", u.owner,
		"uids", uids, "base", base,
		"rows_before", beforeRows, "rows_after", rows,
		"mtime_before", beforeMod, "mtime_after", mod,
		"size_before", beforeSize, "size_after", size)
}

// debugListRead says whether a reader took the list off disk or off the cached
// snapshot the mtime and size validate, and how many rows it got (#1739).
func (u *userMailbox) debugListRead(folder, from string, rows int, mod, size int64) {
	if !listDebug() {
		return
	}
	slog.Debug("maildir: uidlist read",
		"from", from, "user", u.username, "folder", folder, "owner", u.owner,
		"rows", rows, "mtime", mod, "size", size)
}

// listParses counts the times the file was parsed, so "one parse per save" is a
// number a row asserts rather than a claim (#1739).
var listParses atomic.Int64

// ListParses returns the count, ResetListParses zeroes it. Test seams.
func ListParses() int  { return int(listParses.Load()) }
func ResetListParses() { listParses.Store(0) }
