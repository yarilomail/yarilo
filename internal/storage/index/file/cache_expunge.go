package file

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/locks"
)

// The reference's purge_delete_percentage, purge_continued_percentage and
// purge_min_size defaults.
const (
	defaultCachePurgeDeletePct    = 20
	defaultCachePurgeContinuedPct = 200
	defaultCachePurgeMinSize      = 32 * 1024
)

var (
	metricCacheAutoPurge = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fileindex_cache_auto_purge_total",
		Help: "Folder caches purged because their deleted records reached mail_cache_purge_delete_percentage.",
	})
	metricCacheExpungeUncounted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fileindex_cache_expunge_uncounted_total",
		Help: "Commits whose expunged cache records did not reach the cache header; the purge then comes late, never wrong.",
	})
)

// noteCacheExpunged tells the folder's cache of n expunged messages it held a
// record for, after the index hold; a failure only delays the purge.
func (u *userIndex) noteCacheExpunged(folderID uint64, n uint32) {
	if n == 0 {
		return
	}
	fs, err := u.state(folderID)
	if err != nil {
		return
	}
	if err := u.cacheExpunged(fs, folderID, n); err != nil {
		metricCacheExpungeUncounted.Inc()
		slog.Warn("fileindex: expunged cache records not counted", "user", u.username, "folder", fs.folder, "n", n, "err", err)
	}
}

// cacheExpunged moves n records to the cache's deleted count and purges past the
// threshold, in the cache writer's lock order: path mutex, MailboxKey, folder.
func (u *userIndex) cacheExpunged(fs *folderState, folderID uint64, n uint32) error {
	path, err := u.CachePath(folderID)
	if err != nil {
		return err
	}
	release, err := u.holdCachePair(path, fs.folder)
	if err != nil {
		return err
	}
	defer release()
	indexID, resetID, ok, err := u.CachePairIdentity(folderID)
	if err != nil || !ok {
		return err
	}
	cf, err := mailindex.OpenCache(path, indexID, resetID)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, mailindex.ErrCacheInvalid) {
		return nil // nothing a count could describe
	}
	if err != nil {
		return err
	}
	err = cf.ExpungeCount(n)
	_ = cf.Close()
	if err != nil {
		return err
	}
	return u.purgeCacheIfDue(fs, folderID, path)
}

// PurgeCacheIfDue purges the folder's cache when a share reached its threshold,
// as the reference asks on every header write. The caller holds the cache locks.
func (u *userIndex) PurgeCacheIfDue(folderID uint64) error {
	fs, err := u.state(folderID)
	if err != nil {
		return err
	}
	path, err := u.CachePath(folderID)
	if err != nil {
		return err
	}
	return u.purgeCacheIfDue(fs, folderID, path)
}

// purgeCacheIfDue purges when the deleted or the continued share reached its
// threshold on a file of at least the minimum size; a failed purge only waits.
func (u *userIndex) purgeCacheIfDue(fs *folderState, folderID uint64, path string) error {
	indexID, resetID, ok, err := u.CachePairIdentity(folderID)
	if err != nil || !ok {
		return err
	}
	cf, err := mailindex.OpenCache(path, indexID, resetID)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, mailindex.ErrCacheInvalid) {
		return nil
	}
	if err != nil {
		return err
	}
	hdr := cf.Header()
	st, err := os.Stat(path)
	_ = cf.Close()
	if err != nil {
		return err
	}
	var msgs uint32
	if err := u.withFolderROUnlocked(folderID, func(v *folderState) error {
		msgs = uint32(len(v.file.Records))
		return nil
	}); err != nil {
		return err
	}
	deleted := u.b.cachePurgeDeletePct >= 0 && int64(hdr.DeletePercentage(msgs)) >= int64(u.b.cachePurgeDeletePct)
	continued := int64(hdr.ContinuedPercentage(msgs)) >= int64(u.b.cachePurgeContinuedPct)
	if !deleted && !continued || st.Size() < u.b.cachePurgeMinSize {
		return nil
	}
	carried, reclaimed, err := u.PurgeCache(folderID)
	if err != nil {
		slog.Warn("fileindex: cache purge failed", "user", u.username, "folder", fs.folder, "err", err)
		return nil
	}
	metricCacheAutoPurge.Inc()
	slog.Info("fileindex: cache purged", "user", u.username, "folder", fs.folder, "deleted", hdr.DeletedRecordCount,
		"continued", hdr.ContinuedRecordCount, "records", hdr.RecordCount, "carried", carried, "reclaimed_bytes", reclaimed)
	return nil
}

// holdCachePair takes the cache writer's locks, path mutex then MailboxKey; a
// goroutine holding the key exclusive already has every other writer out.
func (u *userIndex) holdCachePair(path, folder string) (func(), error) {
	const site = "cache-expunge"
	if u.b.locker == nil {
		return mailindex.LockCachePath(path, false), nil
	}
	key := locks.MailboxKey(u.username, folder)
	if held, err := locks.Reentrant(u.b.locker, key, site, false); err != nil {
		return nil, err
	} else if held != locks.HoldNone {
		return func() {}, nil
	}
	unlockPath := mailindex.LockCachePath(path, false)
	ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), site), 35*time.Second)
	lk, err := locks.Acquire(ctx, u.b.locker, key, u.owner, 30*time.Second)
	if err != nil {
		cancel()
		unlockPath()
		return nil, fmt.Errorf("fileindex/%s %s: %w", site, folder, err)
	}
	return func() {
		_ = u.b.locker.Unlock(ctx, lk.ID)
		cancel()
		unlockPath()
	}, nil
}
