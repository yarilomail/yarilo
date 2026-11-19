package file

import (
	"fmt"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
)

// A view with no log tail holds the base's lazy chunks (recs), every other
// folderState file.Records; the accessors read either (INDEX-MEMORY.md §3).

func (fs *folderState) recordCount() int {
	if fs.recs != nil {
		return fs.recs.Len()
	}
	return len(fs.file.Records)
}

// loadRecords brings a lazy view's chunks into memory: one open of the base.
// A base replaced since the view was built is mailindex.ErrBaseMoved.
func (fs *folderState) loadRecords() error {
	if fs.recs == nil {
		return nil
	}
	if err := fs.recs.LoadAll(); err != nil {
		return fmt.Errorf("fileindex/records: %w", err)
	}
	return nil
}

func (fs *folderState) recUID(i int) uint32 {
	if fs.recs != nil {
		return fs.recs.UID(i)
	}
	return fs.file.Records[i].UID
}

func (fs *folderState) recFlags(i int) mailindex.MailFlag {
	if fs.recs != nil {
		return fs.recs.Flags(i)
	}
	return fs.file.Records[i].Flags
}

// recExt is the extension's bytes in record i, ok when the layout carries it.
// From a lazy view the bytes are the chunk's: copied by a caller keeping them.
func (fs *folderState) recExt(i int, name string) ([]byte, bool) {
	if fs.recs != nil {
		id, ok := fs.recs.Layout().ExtID(name)
		if !ok {
			return nil, false
		}
		return fs.recs.Ext(i, id), true
	}
	v, ok := fs.file.Records[i].Ext[name]
	return v, ok
}

// materialized is a private copy of the file with its records decoded, for a
// fold to write into.
func (fs *folderState) materialized() (*mailindex.File, error) {
	out := cloneIndexFile(fs.file)
	if fs.recs != nil {
		recs, err := fs.recs.Decode()
		if err != nil {
			return nil, fmt.Errorf("fileindex/records: %w", err)
		}
		out.Records = recs
	}
	return out, nil
}
