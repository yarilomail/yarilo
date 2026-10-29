package mdbox

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
)

// recordStarts walks the file the way framesAt does and returns where each
// record's header begins.
func recordStarts(t *testing.T, d []byte) []int {
	t.Helper()
	var out []int
	pos := bytes.IndexByte(d, '\n') + 1
	for pos < len(d) && d[pos] == magicPreByte0 {
		out = append(out, pos)
		lf := bytes.IndexByte(d[pos:], '\n')
		size, err := parseHeaderSize(d[pos : pos+lf+1])
		if err != nil {
			t.Fatal(err)
		}
		body := pos + lf + 1 + int(size)
		end := bytes.Index(d[body:], []byte("\n\n"))
		if end < 0 {
			break
		}
		pos = body + end + 2
	}
	return out
}

// A file whose only record is at the other size and torn has nothing readable:
// it is removed, and the original stays as .broken.
func TestARewriteWithNothingReadableRemovesTheFile(t *testing.T) {
	u := openTestUserMailbox(t, t.TempDir())
	testSaveMsg(t, u, "Subject: torn\r\n\r\n"+strings.Repeat("torn body line\r\n", 20))
	path := u.mfilePath(1)
	reframeFirstRecord(t, path)
	d := mustRead(t, path)
	hdrEnd := recordStarts(t, d)[0] + bytes.IndexByte(d[recordStarts(t, d)[0]:], '\n') + 1
	torn := d[:hdrEnd+10]
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}

	landed, err := normaliseFrames(path)
	if err != nil {
		t.Fatal(err)
	}
	if landed != nil {
		t.Errorf("landed %v from a file with nothing readable", landed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the empty rewrite was kept: %v", err)
	}
	if !bytes.Equal(mustRead(t, path+brokenSuffix), torn) {
		t.Error("the .broken copy is not the original bytes")
	}
}

// A record whose size field is not a number ends the copy there: the records
// before it are rewritten, the rest stays in the .broken copy.
func TestARewriteStopsAtASizeThatIsNotANumber(t *testing.T) {
	u := openTestUserMailbox(t, t.TempDir())
	testSaveMsg(t, u, "Subject: one\r\n\r\nfirst body\r\n")
	testSaveMsg(t, u, "Subject: two\r\n\r\nsecond body\r\n")
	path := u.mfilePath(1)
	reframeFirstRecord(t, path)
	d := mustRead(t, path)
	starts := recordStarts(t, d)
	if len(starts) != 2 {
		t.Fatalf("setup wants two records, have %d", len(starts))
	}
	copy(d[starts[1]+13:starts[1]+29], "zzzzzzzzzzzzzzzz")
	if err := os.WriteFile(path, d, 0o600); err != nil {
		t.Fatal(err)
	}

	landed, err := normaliseFrames(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(landed) != 1 {
		t.Fatalf("landed %d records, want the one before the bad size", len(landed))
	}
	after := mustRead(t, path)
	if !bytes.Contains(after, []byte("first body")) || bytes.Contains(after, []byte("second body")) {
		t.Errorf("rewritten file: %q", after)
	}
	if !bytes.Contains(mustRead(t, path+brokenSuffix), []byte("second body")) {
		t.Error("the .broken copy lost the record past the bad size")
	}
}

// An earlier .broken is an original some rewrite already replaced: a second
// rewrite would overwrite it, so it refuses, names it, and leaves the file.
func TestARewriteRefusesToOverwriteAnEarlierBrokenCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.14")
	src := mustRead(t, filepath.Join("testdata", "m.shortheader"))
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+brokenSuffix, []byte("earlier original"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := normaliseFrames(path)
	if err == nil || !strings.Contains(err.Error(), path+brokenSuffix) {
		t.Fatalf("err %v, want a refusal naming the .broken copy", err)
	}
	if !bytes.Equal(mustRead(t, path), src) {
		t.Error("the file changed although the rewrite refused")
	}
	if string(mustRead(t, path+brokenSuffix)) != "earlier original" {
		t.Error("the earlier .broken copy was overwritten")
	}
}

// A crashed rewrite leaves its temp copy: the rebuild removes it and never reads
// it as storage, and the mail is untouched.
func TestARebuildRemovesACrashedRewritesTempCopy(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "Subject: kept\r\n\r\nkept\r\n")
	leftover := box.mfilePath(1) + normalisingSuffix
	if err := os.WriteFile(leftover, []byte("half a rewrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(box.storagePath(), "m.99"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := box.RebuildStorage(mailboxbase.Open(box, idx), idx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("the crashed rewrite's copy is still there: %v", err)
	}
	if got := folderCount(t, idx, "INBOX"); got != 1 {
		t.Errorf("INBOX holds %d after the rebuild, want 1", got)
	}
}

// Tagged orphans whose folder is gone bring the folder back, and two of them
// land in the one folder.
func TestRestoredOrphansRecreateTheirFolder(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	box, idx := newBoxAndIndex(t, home)
	deliverMsg(t, box, idx, "INBOX", "Subject: ref\r\n\r\nreferenced\r\n")
	for _, b := range []string{"Subject: p1\r\n\r\none\r\n", "Subject: p2\r\n\r\ntwo\r\n"} {
		if _, _, _, err := box.Save("Projects", strings.NewReader(b), 0, int64(len(b)), nil, nil, [16]byte{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(box.folderPath("Projects")); err != nil {
		t.Fatal(err)
	}

	stats, err := box.RebuildStorage(mailboxbase.Open(box, idx), idx, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OrphansRestored != 2 {
		t.Errorf("restored %d orphans, want 2", stats.OrphansRestored)
	}
	if ok, err := box.FolderExists("Projects"); err != nil || !ok {
		t.Errorf("Projects exists=%v err=%v after the restore", ok, err)
	}
	if got := folderCount(t, idx, "Projects"); got != 2 {
		t.Errorf("Projects holds %d, want 2", got)
	}
}

// A scan reads m.<N> only, or a kept .broken copy counts every message twice;
// an alt tier with nothing moved yet is no error.
func TestAScanReadsStorageFilesOnly(t *testing.T) {
	_, u, _ := altTestBackend(t)
	testSaveMsg(t, u, "Subject: one\r\n\r\none\r\n")
	raw := mustRead(t, u.mfilePath(1))
	for _, name := range []string{"m.1" + brokenSuffix, "m.notanumber"} {
		if err := os.WriteFile(filepath.Join(u.storagePath(), name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(u.altStoragePath()); err != nil {
		t.Fatal(err)
	}
	recs, err := u.Scan("")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Errorf("the scan read %d records, want the one in m.1", len(recs))
	}
}
