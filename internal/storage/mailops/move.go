package mailops

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// MoveRequest moves Msgs from one folder to another, keeping each GUID
// (RFC 8474). Src and Dst are the same Box for a move within one store.
type MoveRequest struct {
	Src, Dst             mailbox.Box
	SrcFolder, DstFolder *mailbox.Folder
	SrcName, DstName     string
	Msgs                 []*mailbox.MessageMeta
	// Read opens a body for a move between stores, where it is copied.
	Read func(*mailbox.MessageMeta) (io.ReadCloser, error)
}

// Moved is one message that reached the destination.
type Moved struct {
	Src, Dst *mailbox.MessageMeta
	DstFile  string
	// srcFile is the copied source body, removed once its record is gone.
	srcFile   string
	relocated bool
}

// MoveIn puts each message into the destination: relocated within one store,
// copied between stores. It stops at the first failure and answers what landed.
func (o *Ops) MoveIn(req MoveRequest) ([]Moved, error) {
	var out []Moved
	for _, m := range req.Msgs {
		if req.Src != req.Dst {
			mv, err := o.moveCopy(req, m)
			if err != nil {
				return out, err
			}
			// Unnamed (a virtual record's copy) is left to the caller.
			mv.srcFile, _ = req.Src.MessagePath(req.SrcName, m)
			out = append(out, mv)
			continue
		}
		srcFile, err := req.Src.MessagePath(req.SrcName, m)
		if err != nil {
			return out, fmt.Errorf("mailops/move path: %w", err)
		}
		name, guid, err := req.Src.Move(req.SrcName, req.DstName, srcFile, m.GUID)
		if err != nil {
			return out, fmt.Errorf("mailops/move relocate: %w", err)
		}
		nm := &mailbox.MessageMeta{
			Flags: m.Flags, Keywords: m.Keywords, Size: m.Size, VSize: m.VSize,
			InternalDate: m.InternalDate, GUID: guid,
		}
		if err := req.Dst.RecordSaved(req.DstFolder, req.DstName, name, nm); err != nil {
			_ = req.Src.Restore(req.SrcName, srcFile, req.DstName, name, nm)
			return out, fmt.Errorf("mailops/move record: %w", err)
		}
		o.Committed(req.DstFolder, locks.EventDelivered, nm.UID, UsageDelta(nm), nm.GUID)
		out = append(out, Moved{Src: m, Dst: nm, DstFile: name, relocated: true})
	}
	return out, nil
}

// moveCopy copies one body between stores; the move does not change usage, so
// the quota is not asked (the reference nets the expunged source against it).
func (o *Ops) moveCopy(req MoveRequest, m *mailbox.MessageMeta) (Moved, error) {
	rc, err := req.Read(m)
	if err != nil {
		return Moved{}, fmt.Errorf("mailops/move fetch: %w", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close() //nolint:errcheck
	if err != nil {
		return Moved{}, fmt.Errorf("mailops/move read: %w", err)
	}
	saved, err := o.Save(req.Dst, req.DstName, req.DstFolder, SaveRequest{
		Body: bytes.NewReader(data), Size: int64(len(data)), Flags: m.Flags, Keywords: m.Keywords,
		Received: m.InternalDate, GUID: m.GUID, QuotaChecked: true,
	})
	if err != nil {
		return Moved{}, fmt.Errorf("mailops/move: %w", err)
	}
	return Moved{Src: m, Dst: saved.Meta, DstFile: saved.Filename}, nil
}

// ExpungeMoved expunges the sources through tx, the transaction they were read
// in (#1805), and answers the uids another session changed meanwhile.
func (o *Ops) ExpungeMoved(req MoveRequest, tx mailbox.BoxTx, moved []Moved) map[uint32]bool {
	kept := o.expungeSource(req, tx, moved)
	for _, mv := range moved {
		if kept[mv.Src.UID] {
			continue
		}
		if !mv.relocated && mv.srcFile != "" {
			if err := req.Src.Remove(req.SrcName, mv.srcFile); err != nil {
				slog.Warn(o.cfg.Protocol+": moved message's source body not removed",
					"folder", req.SrcName, "uid", mv.Src.UID, "err", err)
			}
		}
		o.Committed(req.SrcFolder, locks.EventExpunged, mv.Src.UID, UsageDelta(mv.Src), mv.Src.GUID)
	}
	return kept
}

func (o *Ops) expungeSource(req MoveRequest, tx mailbox.BoxTx, moved []Moved) map[uint32]bool {
	relocated := make(map[uint32]bool, len(moved))
	for _, mv := range moved {
		tx.Expunge(mv.Src.UID)
		relocated[mv.Src.UID] = mv.relocated
	}
	kept := make(map[uint32]bool)
	res, err := tx.Commit()
	if err != nil {
		slog.Warn(o.cfg.Protocol+": MOVE did not expunge the source", "folder", req.SrcName, "err", err)
		for _, mv := range moved {
			kept[mv.Src.UID] = true
		}
		return kept
	}
	// A skipped record whose body was relocated names nothing and must go;
	// one whose body is still there was changed by another session and stays.
	var gone []uint32
	for _, uid := range res.Skipped {
		if relocated[uid] {
			gone = append(gone, uid)
			continue
		}
		kept[uid] = true
		slog.Warn(o.cfg.Protocol+": MOVE left a message another session changed", "folder", req.SrcName, "uid", uid)
	}
	if len(gone) == 0 {
		return kept
	}
	again, err := req.Src.Begin(req.SrcFolder.ID)
	if err == nil {
		for _, uid := range gone {
			again.Expunge(uid)
		}
		_, err = again.Commit()
	}
	if err != nil {
		slog.Warn(o.cfg.Protocol+": MOVE left records whose bodies moved", "folder", req.SrcName, "err", err)
		for _, uid := range gone {
			kept[uid] = true
		}
	}
	return kept
}
