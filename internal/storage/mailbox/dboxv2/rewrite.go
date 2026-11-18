package dboxv2

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// rewritePrefix names a rewrite's temp file: no listing reads it (not u.*),
// and it is not a save's prefix, so neither sweep takes the other's files.
const rewritePrefix = ".yarilo-reencrypt."

var _ mailbox.RecordRewriter = (*userMailbox)(nil)

// Test seams around the replace: after the temp file is verified, and just
// before the rename under the folder lock.
var (
	testAfterRewriteVerified func()
	testBeforeRewriteRename  func()
)

// RewriteRecord replaces u.<uid> around a new body, entries and Z as SaveReceived
// writes them; always synced, as the old body is gone once the rename lands.
func (u *userMailbox) RewriteRecord(folder string, m *mailbox.MessageMeta, rw mailbox.BodyRewrite) (err error) {
	if u.b.locker == nil {
		return errors.New("sdbox/rewrite: no lock service, so another process could not be kept out")
	}
	name, err := u.RecordPath(folder, m)
	if err != nil {
		return err
	}
	dir := u.folderPath(folder)
	path := filepath.Join(dir, name)
	src, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("sdbox/rewrite %s: %w", name, mailbox.ErrExpunged)
		}
		return fmt.Errorf("sdbox/rewrite %s: %w", name, err)
	}
	defer src.Close() //nolint:errcheck
	before, err := src.Stat()
	if err != nil {
		return fmt.Errorf("sdbox/rewrite %s: %w", name, err)
	}
	stamp, body, meta, err := readRecordParts(src, before.Size())
	if err != nil {
		return fmt.Errorf("sdbox/rewrite %s: %w: %w", name, err, mailbox.ErrCorruptStorage)
	}

	var nb bytes.Buffer
	plainLen, sealed, err := rw.Fill(rawSection{SectionReader: body}, &nb)
	if err != nil {
		return fmt.Errorf("sdbox/rewrite %s: %w", name, err)
	}
	meta = withPhysicalSize(meta, plainLen, sealed)

	var out bytes.Buffer
	out.Write(encodeFileHeaderLine(stamp))
	out.Write(encodeMessageHeader(messageHeader{Size: uint64(nb.Len())}))
	out.Write(nb.Bytes())
	out.Write(encodeMetadataBlock(meta))

	tmp := filepath.Join(dir, fmt.Sprintf("%s%d.%d", rewritePrefix, u.b.pid, u.b.tmpSeq.Add(1)))
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err := writeSynced(tmp, out.Bytes()); err != nil {
		return fmt.Errorf("sdbox/rewrite %s: %w", name, err)
	}
	if err := verifyTemp(tmp, rw.Verify); err != nil {
		return fmt.Errorf("sdbox/rewrite %s: %w", name, err)
	}
	if testAfterRewriteVerified != nil {
		testAfterRewriteVerified()
	}
	return u.withMailboxLockSite(folder, "sdbox-rewrite", func() error {
		now, err := os.Stat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("sdbox/rewrite %s: %w", name, mailbox.ErrExpunged)
		case err != nil:
			return fmt.Errorf("sdbox/rewrite %s: %w", name, err)
		case !os.SameFile(now, before) || now.Size() != before.Size() || !now.ModTime().Equal(before.ModTime()):
			return fmt.Errorf("sdbox/rewrite %s: %w", name, mailbox.ErrRecordChanged)
		}
		if testBeforeRewriteRename != nil {
			testBeforeRewriteRename()
		}
		// Dated here, not when written: until now its own mtime is what tells
		// the sweep a live rewrite's temp from a dead one's.
		if err := os.Chtimes(tmp, before.ModTime(), before.ModTime()); err != nil {
			return fmt.Errorf("sdbox/rewrite %s: %w", name, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("sdbox/rewrite %s: %w", name, err)
		}
		if err := syncDirectory(dir); err != nil {
			return fmt.Errorf("sdbox/rewrite %s: sync dir: %w", name, err)
		}
		return nil
	})
}

// readRecordParts splits a u.<uid> file into the create stamp of its header
// line, its body and its metadata entries.
func readRecordParts(f *os.File, size int64) (uint32, *io.SectionReader, []metadataEntry, error) {
	br := bufio.NewReader(io.NewSectionReader(f, 0, size))
	line, err := br.ReadBytes('\n')
	if err != nil {
		return 0, nil, nil, fmt.Errorf("file header: %w", err)
	}
	hdrSize, err := parseFileHeaderSize(line)
	if err != nil {
		return 0, nil, nil, err
	}
	stamp, err := createStamp(line)
	if err != nil {
		return 0, nil, nil, err
	}
	hdr := make([]byte, hdrSize)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return 0, nil, nil, fmt.Errorf("message header: %w", err)
	}
	mh, err := decodeMessageHeader(hdr)
	if err != nil {
		return 0, nil, nil, err
	}
	start := int64(len(line) + hdrSize)
	end := start + int64(mh.Size)
	if end > size {
		return 0, nil, nil, fmt.Errorf("body: %w", io.ErrUnexpectedEOF)
	}
	meta, err := decodeMetadataBlock(io.NewSectionReader(f, end, size-end))
	if err != nil {
		return 0, nil, nil, err
	}
	return stamp, io.NewSectionReader(f, start, int64(mh.Size)), meta, nil
}

// createStamp reads the C field of a file header line.
func createStamp(line []byte) (uint32, error) {
	for _, field := range strings.Fields(string(line)) {
		if len(field) > 1 && field[0] == 'C' {
			n, err := strconv.ParseUint(field[1:], 16, 32)
			if err != nil {
				return 0, fmt.Errorf("file header C field %q: %w", field, err)
			}
			return uint32(n), nil
		}
	}
	return 0, errors.New("file header has no C field")
}

// withPhysicalSize keeps every entry in place and sets Z as SaveReceived
// does: the plaintext's length when sealed, absent when not.
func withPhysicalSize(meta []metadataEntry, plainLen int64, sealed bool) []metadataEntry {
	out := make([]metadataEntry, 0, len(meta)+1)
	seen := false
	for _, e := range meta {
		if e.Key != metaKeyPhysicalSize {
			out = append(out, e)
			continue
		}
		if sealed && !seen {
			out = append(out, metadataEntry{Key: metaKeyPhysicalSize, Value: fmt.Sprintf("%x", plainLen)})
		}
		seen = true
	}
	if sealed && !seen {
		out = append(out, metadataEntry{Key: metaKeyPhysicalSize, Value: fmt.Sprintf("%x", plainLen)})
	}
	return out
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := syncFile(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func verifyTemp(path string, verify func(mailbox.RawBody) error) error {
	f, sec, err := openBody(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	return verify(rawSection{SectionReader: sec})
}

func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck
	return d.Sync()
}

// SweepRewriteTemps removes rewrite temps older than age: younger ones may
// belong to a job still running.
func (u *userMailbox) SweepRewriteTemps(folder string, age time.Duration) (int, error) {
	dir := u.folderPath(folder)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("sdbox/sweep-rewrite: %w", err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), rewritePrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < age {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return n, fmt.Errorf("sdbox/sweep-rewrite: %w", err)
		}
		n++
	}
	return n, nil
}
