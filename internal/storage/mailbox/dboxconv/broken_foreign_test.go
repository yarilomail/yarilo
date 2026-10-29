package dboxconv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxconv"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox/mdboxmap"
)

func write(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Their map index cut short is refused by every reader of their map: an import
// from half a map points folders at messages it never carried.
func TestATruncatedForeignMapIsRefused(t *testing.T) {
	storage := t.TempDir()
	base := dboxref.MapBase(t)
	write(t, storage, map[string][]byte{"dovecot.map.index": base[:40], "dovecot.map.index.log": dboxref.MapBaseLog(t)})
	if _, err := dboxconv.ReadForeignMap(storage); err == nil {
		t.Error("read a map whose index is cut short")
	}
	ours, err := mdboxmap.Open(t.TempDir(), "u")
	if err != nil {
		t.Fatal(err)
	}
	defer ours.Close() //nolint:errcheck
	if _, err := dboxconv.ConvertMap(storage, ours); err == nil {
		t.Error("imported a map whose index is cut short")
	}
	if _, err := dboxconv.NewMapCorrespondence(storage, ours); err == nil {
		t.Error("paired a map whose index is cut short")
	}
}

// A folder naming map uids their map lacks, or a directory with no index of
// theirs, is refused: either converts to messages that point at nothing.
func TestAFolderTheMapCannotPlaceIsRefused(t *testing.T) {
	storage := t.TempDir()
	// Their folder beside a map that carries nothing: its log stops at the header.
	mapLog := dboxref.MapLog(t)
	h, err := dboxindex.ParseLogHeader(mapLog)
	if err != nil {
		t.Fatal(err)
	}
	write(t, storage, map[string][]byte{"dovecot.map.index.log": mapLog[:h.HeaderSize]})
	folder := t.TempDir()
	write(t, folder, map[string][]byte{"dovecot.index": dboxref.IndexBase(t), "dovecot.index.log": dboxref.IndexLog(t)})
	ours, err := mdboxmap.Open(t.TempDir(), "u")
	if err != nil {
		t.Fatal(err)
	}
	defer ours.Close() //nolint:errcheck
	if _, err := dboxconv.ConvertMap(storage, ours); err != nil {
		t.Fatal(err)
	}
	corr, err := dboxconv.NewMapCorrespondence(storage, ours)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dboxconv.ConvertFolder(folder, corr); err == nil {
		t.Error("converted a folder whose map uids the map does not carry")
	}
	if _, _, err := dboxconv.ConvertFolder(t.TempDir(), corr); err == nil {
		t.Error("converted a directory holding no index of theirs")
	}
}
