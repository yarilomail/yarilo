package mdbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

var _ mailbox.StoreRewriter = (*userMailbox)(nil)

// Test seams: between the two holds, and inside the second before the map
// is repointed.
var (
	testBetweenRewriteHolds func()
	testBeforeRewriteMove   func()
	testAfterRewriteMove    func() error
	removeOldFile           = os.Remove
)

// RewriteStore re-encrypts the store one m.<N> at a time, with purge's
// mechanism but the work outside the map lock (MAILCRYPT section 13.7): a
// short hold for the snapshot and the file ids, the copy and its checks with
// no lock, a short hold to compare and repoint. Records with no references
// are carried over, rewritten, as they were.
func (u *userMailbox) RewriteStore(rw mailbox.StoreRewrite) error {
	if u.b.locker == nil {
		return errors.New("mdbox/rewrite: no lock service, so another process could not be kept out")
	}
	m, err := u.openMap()
	if err != nil {
		return err
	}
	if rw.LeftoverAge > 0 {
		if err := u.sweepLeftovers(m, rw.LeftoverAge); err != nil {
			return err
		}
	}
	recs, err := m.RecordsReloaded()
	if err != nil {
		return fmt.Errorf("mdbox/rewrite: records: %w", err)
	}
	files := map[uint32]bool{}
	for _, e := range recs {
		files[e.FileID] = true
	}
	ids := make([]uint32, 0, len(files))
	for id := range files {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if rw.Stop != nil {
			if err := rw.Stop(); err != nil {
				return err
			}
		}
		out, err := u.rewriteFile(m, id, rw)
		if rw.File != nil && (out.Rewritten > 0 || out.Err != nil) {
			rw.File(out)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// rewriteFile returns a hard error (the volume, the map) as err; a file left
// for a reason of its own goes in the outcome.
func (u *userMailbox) rewriteFile(m *mdboxmap.Map, fileID uint32, rw mailbox.StoreRewrite) (mailbox.FileRewrite, error) {
	out := mailbox.FileRewrite{FileID: fileID}
	srcPath, alt, err := u.tierPath(fileID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	if need, err := u.fileNeedsRewrite(m, fileID, srcPath, rw.Needs); err != nil || !need {
		return out, err
	}

	// First hold: the snapshot, the output's id, and a fresh append target
	// so no save lands in the output while it is written unlocked.
	var snap []mdboxmap.MapEntry
	var srcSize int64
	var dstID uint32
	if err := m.Place(func(p *mdboxmap.Placement) error {
		snap = p.RecordsInFile(fileID)
		st, err := os.Stat(srcPath)
		if err != nil {
			return err
		}
		srcSize = st.Size()
		if dstID, err = p.AllocFileID(); err != nil {
			return err
		}
		if _, err := p.AllocFileID(); err != nil {
			return err
		}
		return p.RecordFileCreated(dstID, time.Now().Unix())
	}); err != nil {
		return out, fmt.Errorf("mdbox/rewrite m.%d: first hold: %w", fileID, err)
	}
	if len(snap) == 0 {
		return out, nil
	}
	dstPath := u.mfilePath(dstID)
	if alt {
		dstPath = u.mfileAltPath(dstID)
	}

	moved, err := u.copyRewritten(srcPath, dstPath, dstID, snap, rw, &out)
	if err != nil || out.Err != nil {
		_ = os.Remove(dstPath)
		return out, err
	}
	if testBetweenRewriteHolds != nil {
		testBetweenRewriteHolds()
	}

	// Second hold: the file must be as the snapshot saw it. From the repoint
	// on, the output may be what the map names, whatever error follows: it
	// is never removed after that, as purge never removes its new file.
	repointed := false
	err = m.Place(func(p *mdboxmap.Placement) error {
		now := p.RecordsInFile(fileID)
		st, serr := os.Stat(srcPath)
		if serr != nil || st.Size() != srcSize || !sameRecords(now, snap) {
			return mailbox.ErrRecordChanged
		}
		if testBeforeRewriteMove != nil {
			testBeforeRewriteMove()
		}
		repointed = true
		if err := p.AppendMove(moved, nil); err != nil {
			return err
		}
		if testAfterRewriteMove != nil {
			if err := testAfterRewriteMove(); err != nil {
				return err
			}
		}
		if err := removeOldFile(srcPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			// The map no longer names it: a leftover the next pass sweeps.
			slog.Warn("mdbox/rewrite: the old file stays, the map no longer names it",
				"user", u.username, "file", srcPath, "err", err)
		}
		return nil
	})
	switch {
	case repointed && err != nil:
		return out, fmt.Errorf("mdbox/rewrite m.%d: after the repoint, output m.%d kept: %w", fileID, dstID, err)
	case repointed:
		return out, nil
	case errors.Is(err, mailbox.ErrRecordChanged):
		_ = os.Remove(dstPath)
		out.Err = fmt.Errorf("mdbox/rewrite m.%d: %w", fileID, err)
		out.Rewritten, out.Copied = 0, 0
		return out, nil
	default:
		_ = os.Remove(dstPath)
		return out, fmt.Errorf("mdbox/rewrite m.%d: second hold: %w", fileID, err)
	}
}

// tierPath is where m.<id> lies, and whether that is alt.
func (u *userMailbox) tierPath(fileID uint32) (string, bool, error) {
	p := u.mfilePath(fileID)
	if _, err := os.Stat(p); err == nil {
		return p, false, nil
	} else if !errors.Is(err, os.ErrNotExist) || !u.AltEnabled() {
		return "", false, err
	}
	a := u.mfileAltPath(fileID)
	if _, err := os.Stat(a); err != nil {
		return "", false, err
	}
	return a, true, nil
}

// fileNeedsRewrite probes the file's records with no lock: a file whose
// records are all as wanted is not touched.
func (u *userMailbox) fileNeedsRewrite(m *mdboxmap.Map, fileID uint32, path string, needs func(mailbox.RawBody) (bool, error)) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close() //nolint:errcheck
	for _, e := range m.Records() {
		if e.FileID != fileID {
			continue
		}
		body, _, err := readRecordRaw(f, e.Offset)
		if err != nil {
			return true, nil // the copy reports it, against its record
		}
		if need, err := needs(rawSection{SectionReader: io.NewSectionReader(bytes.NewReader(body), 0, int64(len(body)))}); err != nil || need {
			return true, nil
		}
	}
	return false, nil
}

func (u *userMailbox) copyRewritten(srcPath, dstPath string, dstID uint32, snap []mdboxmap.MapEntry,
	rw mailbox.StoreRewrite, out *mailbox.FileRewrite) ([]mdboxmap.MovedRecord, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return nil, err
	}
	defer src.Close() //nolint:errcheck
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mdbox/rewrite: create m.%d: %w", dstID, err)
	}
	defer dst.Close() //nolint:errcheck
	if _, err := dst.Write(buildDboxFileHeader()); err != nil {
		return nil, err
	}
	pos := int64(len(buildDboxFileHeader()))
	moved := make([]mdboxmap.MovedRecord, 0, len(snap))
	for _, e := range snap {
		body, tr, err := readRecordRaw(src, e.Offset)
		if err != nil {
			out.Err, out.Record = fmt.Errorf("mdbox/rewrite: read map_uid %d: %w", e.UID, err), e.UID
			return nil, nil
		}
		raw := rawSection{SectionReader: io.NewSectionReader(bytes.NewReader(body), 0, int64(len(body)))}
		need, err := rw.Needs(raw)
		if err != nil {
			out.Err, out.Record = err, e.UID
			return nil, nil
		}
		newBody := body
		if need {
			var nb bytes.Buffer
			if _, _, err := rw.Body.Fill(raw, &nb); err != nil {
				out.Err, out.Record = err, e.UID
				return nil, nil
			}
			newBody = nb.Bytes()
			written := rawSection{SectionReader: io.NewSectionReader(bytes.NewReader(newBody), 0, int64(len(newBody)))}
			if err := rw.Body.Verify(written); err != nil {
				out.Err, out.Record = err, e.UID
				return nil, nil
			}
			out.Rewritten++
		} else {
			out.Copied++
		}
		// V as the source had it (a bare-LF body's CRLF count); Z from the
		// new body, as Save derives it.
		rec := buildDboxMessageRecordSized(newBody, tr.guid, tr.origMailbox, messageHeaderSize, tr.internalDate, recSizes{v: tr.vsize})
		if _, err := dst.Write(rec); err != nil {
			return nil, fmt.Errorf("mdbox/rewrite: write m.%d: %w", dstID, err)
		}
		moved = append(moved, mdboxmap.MovedRecord{UID: e.UID, FileID: dstID, Offset: uint32(pos), Size: uint32(len(rec)), GUID: tr.guid})
		pos += int64(len(rec))
	}
	if err := syncFile(dst); err != nil {
		return nil, fmt.Errorf("mdbox/rewrite: fsync m.%d: %w", dstID, err)
	}
	if err := syncDir(filepath.Dir(dstPath)); err != nil {
		return nil, fmt.Errorf("mdbox/rewrite: sync dir: %w", err)
	}
	return moved, nil
}

// readRecordRaw is a record's body and trailer; a trailer that does not parse
// is an error here, where purge only warns, since the rewrite would drop the
// GUID and the date with it.
func readRecordRaw(f *os.File, offset uint32) ([]byte, parsedTrailer, error) {
	bodyOff, size, err := readRecordHeader(f, offset)
	if err != nil {
		return nil, parsedTrailer{}, err
	}
	body := make([]byte, size)
	if _, err := f.ReadAt(body, bodyOff); err != nil {
		return nil, parsedTrailer{}, fmt.Errorf("read body: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		return nil, parsedTrailer{}, err
	}
	end := bodyOff + int64(size)
	_, tr, err := scanTrailer(io.NewSectionReader(f, end, st.Size()-end), 4096)
	if err != nil {
		return nil, parsedTrailer{}, fmt.Errorf("trailer: %w", err)
	}
	return body, tr, nil
}

func sameRecords(a, b []mdboxmap.MapEntry) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(e mdboxmap.MapEntry) [3]uint32 { return [3]uint32{e.UID, e.Offset, e.Size} }
	seen := make(map[[3]uint32]bool, len(a))
	for _, e := range a {
		seen[key(e)] = true
	}
	for _, e := range b {
		if !seen[key(e)] {
			return false
		}
	}
	return true
}

// sweepLeftovers removes, under the map lock, every m.<N> in either tier that
// no record names, past age and not the append target. Under the lock no save
// or purge is between writing a file and naming it, so such a file is what a
// rewrite or purge killed mid-way left; the scan of a rebuild leaves its
// records unnamed, as their GUIDs are the live copies' (pairWithMap).
func (u *userMailbox) sweepLeftovers(m *mdboxmap.Map, age time.Duration) error {
	return m.Place(func(p *mdboxmap.Placement) error {
		dirs := []string{u.storagePath()}
		if u.AltEnabled() {
			dirs = append(dirs, u.altStoragePath())
		}
		for _, dir := range dirs {
			entries, err := os.ReadDir(dir)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("mdbox/rewrite: sweep %s: %w", dir, err)
			}
			for _, e := range entries {
				id, ok := mfileID(e.Name())
				if !ok || id == p.HighestFileID() || len(p.RecordsInFile(id)) > 0 {
					continue
				}
				info, err := e.Info()
				if err != nil || time.Since(info.ModTime()) < age {
					continue
				}
				path := filepath.Join(dir, e.Name())
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("mdbox/rewrite: sweep %s: %w", path, err)
				}
				slog.Info("mdbox/rewrite: removed a file no record names", "user", u.username, "file", path)
			}
		}
		return nil
	})
}

func mfileID(name string) (uint32, bool) {
	if !strings.HasPrefix(name, "m.") {
		return 0, false
	}
	n, err := strconv.ParseUint(name[2:], 10, 32)
	return uint32(n), err == nil
}
