package backendapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"syscall"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/pkg/filelock"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// errNoSpace stops a re-encryption: past a full volume every message would
// fail the same way.
var errNoSpace = errors.New("crypt: the mail volume is full")

// rewriteOutcome is what one message's re-encryption came to.
type rewriteOutcome int

const (
	rewriteDone rewriteOutcome = iota
	rewriteSkipped
	rewriteFailed
)

// bodyRewrite is Fill and Verify for one body: the plaintext hashed on the way
// out of the old body, compared with the hash of the new body read back.
func bodyRewrite(l *mailcrypt.Layer, to string) mailbox.BodyRewrite {
	var want []byte
	var wantLen int64
	return mailbox.BodyRewrite{
		Fill: func(src mailbox.RawBody, dst io.Writer) (int64, bool, error) {
			o, err := l.Open(src, nil)
			if err != nil {
				return 0, false, fmt.Errorf("%w: %w", errRead, err)
			}
			defer o.Close() //nolint:errcheck
			h := sha256.New()
			counted := &countWriter{h: h}
			tee := &readErrs{r: io.TeeReader(o, counted)}
			var werr error
			if to == "plain" {
				_, werr = io.Copy(dst, tee)
			} else {
				werr = l.Seal(dst, tee)
			}
			switch {
			case tee.err != nil:
				return 0, false, fmt.Errorf("%w: %w", errRead, tee.err)
			case werr != nil:
				return 0, false, fmt.Errorf("write: %w", werr)
			}
			want, wantLen = h.Sum(nil), counted.n
			return counted.n, to == "age", nil
		},
		Verify: func(written mailbox.RawBody) error {
			if st, err := l.Probe(written); err != nil {
				return fmt.Errorf("%w: %w", errVerify, err)
			} else if (to == "age" && st != mailcrypt.StateCurrent) || (to == "plain" && st != mailcrypt.StatePlain) {
				return fmt.Errorf("%w: the new body is %s", errVerify, st)
			}
			o, err := l.Open(written, nil)
			if err != nil {
				return fmt.Errorf("%w: %w", errVerify, err)
			}
			defer o.Close() //nolint:errcheck
			h := sha256.New()
			n, err := io.Copy(h, o)
			if err != nil {
				return fmt.Errorf("%w: %w", errVerify, err)
			}
			if verifyHook != nil {
				verifyHook(h)
			}
			if n != wantLen || !bytes.Equal(h.Sum(nil), want) {
				return fmt.Errorf("%w: the new body's plaintext differs from the old one's", errVerify)
			}
			return nil
		},
	}
}

// rewriteBody re-encrypts one message through the driver, which puts it in
// place under its lock once Verify has passed.
func rewriteBody(l *mailcrypt.Layer, rr mailbox.RecordRewriter, driver, user, ns, folder string,
	m *mailbox.MessageMeta, from mailcrypt.State, to string, recorded func() (bool, error)) (rewriteOutcome, error) {
	rw := bodyRewrite(l, to)
	rw.Recorded = recorded
	err := rr.RewriteRecord(folder, m, rw)
	reason := ""
	switch {
	case err == nil:
		mailcrypt.CountReencrypted(driver, from)
		return rewriteDone, nil
	case errors.Is(err, syscall.ENOSPC):
		mailcrypt.CountReencryptFailed(driver, "nospace")
		slog.Error("backendapi/crypt: no space left, the run stops", "user", user, "namespace", ns, "folder", folder, "uid", m.UID, "err", err)
		return rewriteFailed, errNoSpace
	case errors.Is(err, mailbox.ErrExpunged):
		mailcrypt.CountReencryptFailed(driver, "vanished")
		return rewriteSkipped, nil
	case errors.Is(err, filelock.ErrNoSharedLock):
		mailcrypt.CountReencryptFailed(driver, "nolock")
		return rewriteFailed, fmt.Errorf("crypt: %s needs flock on the mail volume to keep the sessions' renames out "+
			"(storage_lock_method: flock, and a mount that locks on the server): %w", driver, err)
	case errors.Is(err, mailbox.ErrRecordChanged):
		reason = "changed"
	case errors.Is(err, mailbox.ErrRecordBusy):
		reason = "busy"
	case errors.Is(err, errVerify):
		reason = "verify"
	case errors.Is(err, errRead):
		reason = "read"
	default:
		reason = "write"
	}
	mailcrypt.CountReencryptFailed(driver, reason)
	slog.Error("backendapi/crypt: message left as it was", "user", user, "namespace", ns, "folder", folder,
		"uid", m.UID, "reason", reason, "err", err)
	return rewriteFailed, nil
}

var (
	errVerify = errors.New("crypt: verification failed")
	errRead   = errors.New("crypt: the old body could not be read")
)

// readErrs keeps the source's read error apart from the writer's, which the
// copy reports alike.
type readErrs struct {
	r   io.Reader
	err error
}

func (r *readErrs) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

// verifyHook alters the hash of the read-back body. Test seam: a verification
// that passes a corrupted body must turn a row red.
var verifyHook func(hash.Hash)

type countWriter struct {
	h hash.Hash
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.h.Write(p)
}

// rewriteStore runs a store's file-by-file rewrite; a file the map moved
// under is taken again on the next pass, up to three.
func (s *Server) rewriteStore(ctx context.Context, job *cryptJob, req cryptRequest, ns, driver string,
	sw mailbox.StoreRewriter, layer *mailcrypt.Layer) error {
	row := job.addFolder(ns, "(store files)")
	for pass := 0; pass < 3; pass++ {
		changed := 0
		var from mailcrypt.State
		rw := mailbox.StoreRewrite{
			Needs: func(b mailbox.RawBody) (bool, error) {
				st, err := layer.Probe(b)
				if err != nil {
					return false, fmt.Errorf("%w: %w", errRead, err)
				}
				if st == mailcrypt.StateNoKey {
					return false, nil
				}
				from = st
				return needsRewrite(st, req.To), nil
			},
			Body: bodyRewrite(layer, req.To),
			Stop: ctx.Err,
		}
		rw.File = func(f mailbox.FileRewrite) {
			if f.Err == nil {
				for range f.Rewritten {
					mailcrypt.CountReencrypted(driver, from)
				}
				job.rewroteMany(f.Rewritten)
				return
			}
			reason := "write"
			switch {
			case errors.Is(f.Err, mailbox.ErrRecordChanged):
				reason = "changed"
				changed++
			case errors.Is(f.Err, errVerify):
				reason = "verify"
			case errors.Is(f.Err, errRead):
				reason = "read"
			}
			mailcrypt.CountReencryptFailed(driver, reason)
			if reason != "changed" {
				job.rewrote(row, rewriteFailed)
			}
			slog.Error("backendapi/crypt: store file left as it was", "user", req.User, "namespace", ns,
				"file_id", f.FileID, "map_uid", f.Record, "reason", reason, "err", f.Err)
		}
		if err := sw.RewriteStore(rw); err != nil {
			if errors.Is(err, syscall.ENOSPC) {
				mailcrypt.CountReencryptFailed(driver, "nospace")
				return errNoSpace
			}
			return err
		}
		if changed == 0 {
			return nil
		}
	}
	return errors.New("crypt: store files kept changing under the rewrite; run it again")
}
