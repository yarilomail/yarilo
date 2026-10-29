package jmap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailops"
	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The upload store's lock, as the thread recorder's: a request would rather
// wait than fail, and a lock nobody releases must not hold an account.
const (
	uploadLockWait  = 10 * time.Second
	uploadLockTTL   = 15 * time.Second
	uploadLockRenew = 5 * time.Second
)

var errNoBlob = errors.New("jmap: no such blob")

// uploadID names an uploaded blob by content (INTERNALS §39).
type uploadID struct {
	sum  [sha256.Size]byte
	size int64
}

func (u uploadID) String() string {
	return "U" + hex.EncodeToString(u.sum[:]) + "-" + strconv.FormatInt(u.size, 10)
}

// parseUploadID accepts only the form String makes, with a size up to max;
// nothing else ever reaches a path.
func parseUploadID(s string, max int64) (uploadID, bool) {
	rest, ok := strings.CutPrefix(s, "U")
	if !ok {
		return uploadID{}, false
	}
	sum, size, ok := strings.Cut(rest, "-")
	if !ok || len(sum) != 2*sha256.Size || strings.ToLower(sum) != sum || size == "" || (len(size) > 1 && size[0] == '0') {
		return uploadID{}, false
	}
	var id uploadID
	if _, err := hex.Decode(id.sum[:], []byte(sum)); err != nil {
		return uploadID{}, false
	}
	n, err := strconv.ParseInt(size, 10, 64)
	if err != nil || n < 0 || n > max {
		return uploadID{}, false
	}
	id.size = n
	return id, true
}

// uploadStore is one account's uploaded blobs.
type uploadStore struct {
	dir, user, owner string
	locker           locks.Locker
	ttl              time.Duration
}

func (h *userHandle) uploads(locker locks.Locker, ttl time.Duration) (*uploadStore, error) {
	root := mailbox.ControlRoot(h.info)
	if root == "" {
		return nil, fmt.Errorf("jmap: no control root for %s", h.info.Username)
	}
	return &uploadStore{
		dir:    filepath.Join(root, "jmap-blobs", "tmp"),
		user:   h.info.Username,
		owner:  locks.Owner(h.info.Username, h.info.LockID()),
		locker: locker,
		ttl:    ttl,
	}, nil
}

func (st *uploadStore) locked(ctx context.Context, fn func() error) error {
	if st.locker == nil {
		return fn()
	}
	return locks.WithLockWaiting(locks.WithSite(ctx, "jmap-upload"), st.locker, "jmapblobs:"+st.user, st.owner,
		uploadLockTTL, uploadLockRenew, uploadLockWait, func(context.Context) error { return fn() })
}

// put stores r, at most max bytes; errTooLarge past it.
func (st *uploadStore) put(ctx context.Context, r io.Reader, max int64) (uploadID, error) {
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		return uploadID{}, fmt.Errorf("jmap/upload mkdir: %w", err)
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	part := filepath.Join(st.dir, "."+hex.EncodeToString(rnd[:])+".part")
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return uploadID{}, fmt.Errorf("jmap/upload create: %w", err)
	}
	defer os.Remove(part) //nolint:errcheck // gone after the rename
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(r, max+1))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return uploadID{}, fmt.Errorf("jmap/upload write: %w", err)
	}
	if n > max {
		return uploadID{}, errTooLarge
	}
	id := uploadID{size: n}
	copy(id.sum[:], hash.Sum(nil))
	err = st.locked(ctx, func() error {
		final := filepath.Join(st.dir, id.String())
		if _, serr := os.Stat(final); serr == nil {
			now := time.Now()
			return os.Chtimes(final, now, now)
		}
		if rerr := os.Rename(part, final); rerr != nil {
			return rerr
		}
		st.sweepLocked()
		return nil
	})
	if err != nil {
		return uploadID{}, fmt.Errorf("jmap/upload store: %w", err)
	}
	return id, nil
}

// open hands back an open blob; a missing, expired or mis-sized one is errNoBlob.
func (st *uploadStore) open(ctx context.Context, id uploadID) (*os.File, error) {
	var f *os.File
	err := st.locked(ctx, func() error {
		st.sweepLocked()
		var oerr error
		f, oerr = os.Open(filepath.Join(st.dir, id.String()))
		if oerr != nil {
			return errNoBlob
		}
		if fi, serr := f.Stat(); serr != nil || fi.Size() != id.size {
			f.Close() //nolint:errcheck
			f = nil
			return errNoBlob
		}
		return nil
	})
	return f, err
}

// pending is the size of the account's live blobs.
func (st *uploadStore) pending() int64 {
	entries, _ := os.ReadDir(st.dir)
	var total int64
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || strings.HasPrefix(e.Name(), ".") || time.Since(fi.ModTime()) > st.ttl {
			continue
		}
		total += fi.Size()
	}
	return total
}

// sweepLocked removes blobs and part files older than the TTL.
func (st *uploadStore) sweepLocked() {
	entries, _ := os.ReadDir(st.dir)
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || time.Since(fi.ModTime()) <= st.ttl {
			continue
		}
		if rerr := os.Remove(filepath.Join(st.dir, e.Name())); rerr != nil && !os.IsNotExist(rerr) {
			slog.Warn("jmap: upload sweep", "user", st.user, "file", e.Name(), "err", rerr)
		}
	}
}

var errTooLarge = errors.New("jmap: upload over maxSizeUpload")

// handleUpload stores one blob for the caller (RFC 8620 §6.1).
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, id identity) {
	accountID := strings.Trim(strings.TrimPrefix(r.URL.Path, jmapcore.UploadPath), "/")
	if accountID != jmapcore.AccountID(id.user) {
		jmapcore.WriteProblem(w, http.StatusNotFound, "No such account")
		return
	}
	if s.opts.Storage == nil {
		jmapcore.WriteProblem(w, http.StatusServiceUnavailable, "Mail store unavailable")
		return
	}
	max := s.uploadMax()
	if r.ContentLength > max {
		writeLimit(w, "maxSizeUpload")
		return
	}
	h, err := s.opts.Storage.open(id.user, id.sessionID)
	if err != nil {
		slog.Warn("jmap: upload store open failed", "user", id.user, "err", err)
		jmapcore.WriteProblem(w, http.StatusServiceUnavailable, "Mail store unavailable")
		return
	}
	defer h.close()
	st, err := h.uploads(s.opts.Storage.Locker, s.opts.UploadTTL)
	if err != nil {
		jmapcore.WriteProblem(w, http.StatusServiceUnavailable, "Mail store unavailable")
		return
	}
	// Without a length the body is counted at the most it can be (§39); with
	// no cap there is no such number.
	size := r.ContentLength
	if size < 0 {
		size = 0
		if s.opts.Limits.MaxSizeUpload > 0 {
			size = max
		}
	}
	var refused *mailops.RefusedError
	if errors.As(h.ops.Quota.CheckUpload(st.pending()+size), &refused) {
		jmapcore.WriteProblem(w, http.StatusInsufficientStorage, "Account is over quota")
		return
	}
	blob, err := st.put(r.Context(), r.Body, max)
	switch {
	case errors.Is(err, errTooLarge):
		writeLimit(w, "maxSizeUpload")
		return
	case err != nil:
		slog.Warn("jmap: upload failed", "user", id.user, "err", err)
		jmapcore.WriteProblem(w, http.StatusServiceUnavailable, "Upload not stored")
		return
	}
	ctype := r.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accountId": jmapcore.AccountID(id.user), "blobId": blob.String(), "type": ctype, "size": blob.size,
	})
}

// uploadMax is jmap_max_size_upload; 0 is no cap, as at the login edge.
func (s *Server) uploadMax() int64 {
	if m := s.opts.Limits.MaxSizeUpload; m > 0 {
		return m
	}
	return math.MaxInt64 - 1
}

func writeLimit(w http.ResponseWriter, limit string) {
	(&jmapcore.RequestError{
		Type: jmapcore.ProblemLimit, Status: http.StatusRequestEntityTooLarge,
		Limit: limit, Detail: "The upload exceeds " + limit + ".",
	}).Write(w)
}
