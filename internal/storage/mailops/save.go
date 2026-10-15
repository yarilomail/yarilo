package mailops

import (
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// SaveRequest is one message to store.
type SaveRequest struct {
	Body io.Reader
	Size int64
	// Flags are system flags, Keywords the rest.
	Flags, Keywords []string
	// Received is the INTERNALDATE; zero takes the time of the save.
	Received time.Time
	// QuotaFolder is the name the quota rules see; empty is the folder saved to.
	QuotaFolder string
	// GUID keeps a copy's identity (RFC 8474 §5.1); zero mints a new one.
	GUID [16]byte
	// QuotaChecked is a save whose batch the caller checked with CheckBatch.
	QuotaChecked bool
}

// Saved is what a save settled.
type Saved struct {
	Meta     *mailbox.MessageMeta
	Folder   *mailbox.Folder
	Filename string
}

// ShortBodyError is a body that ended before its declared size. Nothing is
// kept unless Orphaned: the partial body could not be removed.
type ShortBodyError struct {
	Got, Want int64
	Orphaned  bool
}

func (e *ShortBodyError) Error() string {
	return fmt.Sprintf("body under-delivered: %d of %d octets", e.Got, e.Want)
}

// UsageDelta is the size a message moves the quota by: its CRLF-counted size,
// or the stored one for a record written before that was known.
func UsageDelta(m *mailbox.MessageMeta) uint32 {
	if m.VSize > 0 {
		return m.VSize
	}
	return m.Size
}

// Save stores one message into folder of box, records it and runs what follows
// a commit. box may be another namespace's; the quota and the events stay this
// session's. A delivery's quota was checked before its filter ran.
func (o *Ops) Save(box mailbox.Box, folder string, f *mailbox.Folder, req SaveRequest) (Saved, error) {
	if !o.Quota.delivery && !req.QuotaChecked {
		qf := req.QuotaFolder
		if qf == "" {
			qf = folder
		}
		if err := o.Quota.CheckSave(qf, req.Size); err != nil {
			return Saved{}, err
		}
	}
	if f == nil {
		var err error
		if f, err = box.Folder(folder, 0); err != nil {
			return Saved{}, fmt.Errorf("mailops/save open: %w", err)
		}
	}
	tSave := time.Now()
	// The store copies to EOF without checking the count, so a short body
	// would be kept truncated and answered as stored (#1129).
	counted := &countingReader{r: req.Body}
	received := req.Received
	var name string
	var vsize uint32
	var guid [16]byte
	var err error
	if received.IsZero() {
		received = time.Now()
		name, vsize, guid, err = box.Save(folder, counted, 0, req.Size, req.Flags, req.Keywords, req.GUID)
	} else {
		name, vsize, guid, err = box.SaveReceived(folder, counted, 0, req.Size, req.Flags, req.Keywords, req.GUID, received)
	}
	if err != nil {
		return Saved{Folder: f}, fmt.Errorf("mailops/save: %w", err)
	}
	if counted.n != req.Size {
		short := &ShortBodyError{Got: counted.n, Want: req.Size}
		if rmErr := box.Discard(folder, name, nil); rmErr != nil {
			short.Orphaned = true
			slog.Error(o.cfg.Protocol+": short body kept, removal failed",
				"folder", folder, "file", name, "err", rmErr)
		}
		return Saved{Folder: f}, short
	}
	tIndex := time.Now()
	m := &mailbox.MessageMeta{
		Flags: req.Flags, Keywords: req.Keywords, Size: uint32(req.Size), VSize: vsize,
		InternalDate: received, GUID: guid,
	}
	if err := box.RecordSaved(f, folder, name, m); err != nil {
		_ = box.Discard(folder, name, m)
		return Saved{Folder: f}, fmt.Errorf("mailops/save record: %w", err)
	}
	// The driver settles the name inside the record cycle; ask it.
	if named, nerr := box.MessagePath(folder, m); nerr == nil {
		name = named
	}
	slog.Debug(o.cfg.Protocol+": save timing", "folder", folder, "uid", m.UID, "size", req.Size,
		"save_ms", tIndex.Sub(tSave).Milliseconds(), "index_ms", time.Since(tIndex).Milliseconds())
	o.Committed(f, locks.EventDelivered, m.UID, UsageDelta(m), m.GUID)
	return Saved{Meta: m, Folder: f, Filename: name}, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
