package backendapi

import (
	"bytes"
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

// rewriteBody re-encrypts one message through the driver: plaintext hashed on
// the way out of the old body, compared with the hash of the new body read
// back, and only then put in place by the driver under its lock.
func rewriteBody(l *mailcrypt.Layer, rr mailbox.RecordRewriter, driver, user, ns, folder string,
	m *mailbox.MessageMeta, from mailcrypt.State, to string, recorded func() (bool, error)) (rewriteOutcome, error) {
	var want []byte
	var wantLen int64
	rw := mailbox.BodyRewrite{
		Recorded: recorded,
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
