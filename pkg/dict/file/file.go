// Package file is a JSON-backed local-filesystem dict driver.
//
// One Dict instance maps to one file path, passed in Config.Settings["path"];
// per-user templating (%u/%h/%n/%d/%i) is the caller's job via pkg/dict/varexpand
// before Open.
//
// The path may name variables (%u/%h/%n/%d/%i); they are expanded per operation
// from OpSettings, so one Dict serves every user with a file of their own.
//
// Several processes may hold the same file: a write takes pkg/filelock and
// re-reads under it, and a read reloads when the file's stamp moved. Reads take
// no lock.
//
// Format: one JSON document with a fixed envelope and an array of rows; the
// "version" tag is reserved for future migrations. []byte values are
// base64-encoded by encoding/json, so binary values survive round-trip.
//
// Writes are atomic via temp-file + fsync + rename: the on-disk file is either
// the pre-write or post-write state, never partial.
package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/varexpand"
	"github.com/yarilomail/yarilo/pkg/filelock"
)

// DriverName is how this driver is named in config, and what a caller matches
// on to decide it can open the dict itself.
const DriverName = "file"

func init() {
	dict.Register(DriverName, New)
}

// New constructs a file dict. Required setting: "path" (string). The file need
// not exist yet — the first commit creates it and any missing parent
// directories (0700).
func New(cfg dict.Config) (dict.Dict, error) {
	pathAny, ok := cfg.Settings["path"]
	if !ok {
		return nil, errors.New("missing required setting \"path\"")
	}
	path, ok := pathAny.(string)
	if !ok || path == "" {
		return nil, fmt.Errorf("setting \"path\" must be a non-empty string, got %T", pathAny)
	}
	method, err := filelock.Parse(settingString(cfg.Settings, "lock_method"))
	if err != nil {
		return nil, err
	}
	return &Dict{tmpl: path, method: method, stores: map[string]*store{}}, nil
}

// settingString reads an optional string setting; a non-string is left to the
// caller's parser to reject.
func settingString(settings map[string]any, key string) string {
	if v, ok := settings[key].(string); ok {
		return v
	}
	return ""
}

// lockWait is the same bound the uidlist writer uses: long enough for a slow
// volume, short enough that a wedged holder is reported rather than waited on.
const lockWait = 30 * time.Second

const formatVersion = 1

type Dict struct {
	tmpl   string
	method filelock.Method
	mu     sync.Mutex
	stores map[string]*store
	closed atomic.Bool
}

// store is one file: the rows it held when we last read it, and the stamp they
// came from. Another process writing the file moves the stamp.
type store struct {
	path   string
	method filelock.Method
	mu     sync.Mutex
	rows   map[string]row
	loaded bool
	stamp  string
}

// storeFor resolves the path for this operation's user. A template naming %h
// with no home in the settings is a configuration error at the call, not an
// empty path silently shared by everyone.
func (d *Dict) storeFor(set *dict.OpSettings) (*store, error) {
	vars := varexpand.Vars{}
	if set != nil {
		vars.Username, vars.HomeDir = set.Username, set.HomeDir
	}
	if strings.Contains(d.tmpl, "%h") && vars.HomeDir == "" {
		return nil, fmt.Errorf("file: path %q needs the home of %q and the operation carries none", d.tmpl, vars.Username)
	}
	path := varexpand.Expand(d.tmpl, vars)
	if path == "" {
		return nil, fmt.Errorf("file: path %q expands to nothing for user %q", d.tmpl, vars.Username)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.stores[path]
	if !ok {
		st = &store{path: path, method: d.method, rows: map[string]row{}}
		d.stores[path] = st
	}
	return st, nil
}

type row struct {
	Values  [][]byte `json:"v"`
	Expires int64    `json:"exp,omitempty"` // unix seconds; 0 = no TTL
}

type envelope struct {
	Version int         `json:"version"`
	Entries []wireEntry `json:"entries"`
}

type wireEntry struct {
	Key string `json:"k"`
	row
}

func (d *Dict) Name() string                 { return "file" }
func (d *Dict) Wait(_ context.Context) error { return nil }

func (d *Dict) Close() error {
	d.closed.Store(true)
	d.mu.Lock()
	d.stores = map[string]*store{}
	d.mu.Unlock()
	return nil
}

// loadLocked reads the on-disk file into d.rows once; subsequent calls are a
// no-op. d.mu (write) must be held.
// stampOf names the file's state cheaply. Another process's write lands as a
// rename, so size and mtime together move whenever the content does.
func stampOf(fi os.FileInfo) string {
	return strconv.FormatInt(fi.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(fi.Size(), 10)
}

// loadLocked reads the file when this process has not read it, or when someone
// else has written it since. s.mu must be held.
func (s *store) loadLocked() error {
	fi, statErr := os.Stat(s.path)
	if statErr == nil && s.loaded && stampOf(fi) == s.stamp {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.rows = map[string]row{}
			s.loaded, s.stamp = true, ""
			return nil
		}
		return fmt.Errorf("read: %w", err)
	}
	rows := make(map[string]row, len(s.rows))
	if len(data) > 0 {
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		for _, e := range env.Entries {
			rows[e.Key] = e.row
		}
	}
	s.rows = rows
	s.loaded = true
	s.stamp = ""
	if fi, err := os.Stat(s.path); err == nil {
		s.stamp = stampOf(fi)
	}
	return nil
}

// withWriteLock runs f holding the file lock, with the rows re-read under it:
// the whole file is rewritten on every commit, so a writer that did not re-read
// would drop whatever another process wrote meanwhile.
func (s *store) withWriteLock(f func() error) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	h, err := filelock.Take(s.path+".lock", s.method, lockWait)
	if err != nil {
		return fmt.Errorf("file: lock %s: %w", s.path, err)
	}
	// A failed release says nothing a caller can act on: the process either
	// exits or takes the lock again.
	defer func() { _ = h.Release() }()
	s.loaded = false
	if err := s.loadLocked(); err != nil {
		return err
	}
	return f()
}

// flushLocked serialises d.rows and writes it atomically via temp-file + fsync +
// rename. s.mu and the file lock must be held; s.rows must already be the
// desired post-write state.
func (s *store) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}

	entries := make([]wireEntry, 0, len(s.rows))
	for k, r := range s.rows {
		entries = append(entries, wireEntry{Key: k, row: r})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })

	data, err := json.MarshalIndent(envelope{Version: formatVersion, Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck — best-effort cleanup if rename succeeds.

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	s.stamp = ""
	if fi, err := os.Stat(s.path); err == nil {
		s.stamp = stampOf(fi)
	}
	return nil
}

func (d *Dict) Lookup(ctx context.Context, set *dict.OpSettings, key string) ([][]byte, bool, error) {
	if err := d.guard(ctx); err != nil {
		return nil, false, err
	}
	st, err := d.storeFor(set)
	if err != nil {
		return nil, false, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return nil, false, err
	}
	r, ok := st.rows[key]
	if !ok {
		return nil, false, nil
	}
	if r.Expires > 0 && time.Now().Unix() > r.Expires {
		return nil, false, nil
	}
	out := make([][]byte, len(r.Values))
	for i, v := range r.Values {
		out[i] = append([]byte(nil), v...)
	}
	return out, true, nil
}

func (d *Dict) Iterate(ctx context.Context, set *dict.OpSettings, path string, flags dict.IterFlag) (dict.Iterator, error) {
	if err := d.guard(ctx); err != nil {
		return nil, err
	}
	st, err := d.storeFor(set)
	if err != nil {
		return nil, err
	}
	recurse := flags&dict.IterRecurse != 0
	exactKey := flags&dict.IterExactKey != 0
	noValue := flags&dict.IterNoValue != 0
	sortByKey := flags&dict.IterSortByKey != 0
	sortByValue := flags&dict.IterSortByValue != 0

	st.mu.Lock()
	if err := st.loadLocked(); err != nil {
		st.mu.Unlock()
		return nil, err
	}
	now := time.Now().Unix()
	var rows []iterRow
	for k, r := range st.rows {
		if r.Expires > 0 && now > r.Expires {
			continue
		}
		if !dict.PathMatches(path, k, recurse, exactKey) {
			continue
		}
		rr := iterRow{key: k}
		if !noValue {
			rr.values = make([][]byte, len(r.Values))
			for i, v := range r.Values {
				rr.values[i] = append([]byte(nil), v...)
			}
		}
		rows = append(rows, rr)
	}
	st.mu.Unlock()

	switch {
	case sortByKey:
		sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	case sortByValue:
		sort.Slice(rows, func(i, j int) bool {
			a, b := "", ""
			if len(rows[i].values) > 0 {
				a = string(rows[i].values[0])
			}
			if len(rows[j].values) > 0 {
				b = string(rows[j].values[0])
			}
			return a < b
		})
	}
	return &iterator{rows: rows, idx: -1}, nil
}

func (d *Dict) Begin(ctx context.Context, set *dict.OpSettings) (dict.Tx, error) {
	if err := d.guard(ctx); err != nil {
		return nil, err
	}
	st, err := d.storeFor(set)
	if err != nil {
		return nil, err
	}
	expire := int64(0)
	if set != nil && set.ExpireSecs > 0 {
		expire = time.Now().Unix() + int64(set.ExpireSecs)
	}
	return &tx{d: d, st: st, expires: expire}, nil
}

// ExpireScan sweeps every file this process has opened; a file nobody here has
// touched is swept by whoever does touch it.
func (d *Dict) ExpireScan(ctx context.Context) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	stores := make([]*store, 0, len(d.stores))
	for _, st := range d.stores {
		stores = append(stores, st)
	}
	d.mu.Unlock()
	for _, st := range stores {
		if err := st.expire(); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) expire() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withWriteLock(func() error {
		now := time.Now().Unix()
		changed := false
		for k, r := range s.rows {
			if r.Expires > 0 && now > r.Expires {
				delete(s.rows, k)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return s.flushLocked()
	})
}

func (d *Dict) guard(ctx context.Context) error {
	if d.closed.Load() {
		return dict.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// ----- iterator -----

type iterRow struct {
	key    string
	values [][]byte
}

type iterator struct {
	rows []iterRow
	idx  int
	err  error
}

func (it *iterator) Next() bool {
	it.idx++
	return it.idx < len(it.rows)
}

func (it *iterator) Key() string { return it.rows[it.idx].key }

func (it *iterator) Values() [][]byte {
	if it.idx < 0 || it.idx >= len(it.rows) {
		return nil
	}
	return it.rows[it.idx].values
}

func (it *iterator) Err() error   { return it.err }
func (it *iterator) Close() error { return nil }

// ----- tx -----

type tx struct {
	d       *Dict
	st      *store
	buf     dict.MemoryTx
	expires int64
	done    bool
}

func (t *tx) Set(key string, value []byte) error {
	if t.done {
		return errors.New("file: tx already finalised")
	}
	t.buf.Set(key, value)
	return nil
}

func (t *tx) Unset(key string) error {
	if t.done {
		return errors.New("file: tx already finalised")
	}
	t.buf.Unset(key)
	return nil
}

func (t *tx) AtomicInc(key string, delta int64) error {
	if t.done {
		return errors.New("file: tx already finalised")
	}
	t.buf.AtomicInc(key, delta)
	return nil
}

func (t *tx) Rollback() error {
	t.done = true
	t.buf.Reset()
	return nil
}

func (t *tx) Commit() (dict.CommitResult, error) {
	if t.done {
		return dict.CommitFailed, errors.New("file: tx already finalised")
	}
	t.done = true
	if t.d.closed.Load() {
		return dict.CommitFailed, dict.ErrClosed
	}

	st := t.st
	st.mu.Lock()
	defer st.mu.Unlock()

	// The file lock spans the re-read and the write: a commit rewrites the
	// whole file, so anything another process wrote has to be read first.
	result := dict.CommitOK
	err := st.withWriteLock(func() error {
		res, err := t.applyLocked(st)
		result = res
		return err
	})
	if err != nil {
		return dict.CommitFailed, err
	}
	return result, nil
}

// applyLocked applies the buffered ops to a snapshot and writes it. s.mu and
// the file lock are held, and st.rows has just been re-read from disk.
func (t *tx) applyLocked(st *store) (dict.CommitResult, error) {
	// Apply ops to a snapshot — only commit to st.rows after every op
	// succeeds, so a failing atomic-inc does not leave a half-applied
	// transaction in memory.
	snap := make(map[string]row, len(st.rows))
	for k, v := range st.rows {
		snap[k] = v
	}

	for _, op := range t.buf.Ops {
		switch op.Kind {
		case dict.OpSet:
			r := row{Values: [][]byte{append([]byte(nil), op.Value...)}, Expires: t.expires}
			snap[op.Key] = r
		case dict.OpUnset:
			delete(snap, op.Key)
		case dict.OpAtomicInc:
			r, ok := snap[op.Key]
			if !ok || len(r.Values) == 0 {
				return dict.CommitNotFound, nil
			}
			n, err := strconv.ParseInt(string(r.Values[0]), 10, 64)
			if err != nil {
				return dict.CommitFailed, fmt.Errorf("file: atomic-inc on non-integer value at %q", op.Key)
			}
			r.Values = [][]byte{[]byte(strconv.FormatInt(n+op.Delta, 10))}
			if t.expires > 0 {
				r.Expires = t.expires
			}
			snap[op.Key] = r
		}
	}

	// Promote snapshot then flush. On flush failure, the in-memory map
	// is rewound so subsequent reads still see the pre-commit state.
	prev := st.rows
	st.rows = snap
	if err := st.flushLocked(); err != nil {
		st.rows = prev
		return dict.CommitFailed, err
	}
	return dict.CommitOK, nil
}
