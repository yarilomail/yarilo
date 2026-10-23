package sieve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/locks"
)

// ScriptIndex is a user's script state counter and the stable id of each
// script (INTERNALS §37, RFC 9661 §2.1: an id survives a rename).
type ScriptIndex struct {
	State uint64            `json:"state"`
	Next  uint64            `json:"next"`
	IDs   map[string]string `json:"ids"`
}

// NameOf returns the script name carrying id.
func (ix ScriptIndex) NameOf(id string) (string, bool) {
	for name, sid := range ix.IDs {
		if sid == id {
			return name, true
		}
	}
	return "", false
}

func (ix *ScriptIndex) assign(name string) {
	if _, ok := ix.IDs[name]; !ok {
		ix.Next++
		ix.IDs[name] = "S" + strconv.FormatUint(ix.Next, 10)
	}
}

// indexIO keeps one user's ScriptIndex; update runs fn under the index lock
// and writes the result when fn reports a change.
type indexIO interface {
	update(ctx context.Context, username, homeDir string, fn func(context.Context, *ScriptIndex) (bool, error)) (ScriptIndex, error)
}

// IndexedStore is a ScriptStore whose every change moves the user's state
// counter under the index lock, so ManageSieve and JMAP agree on one state.
type IndexedStore struct {
	ScriptStore
	io     indexIO
	locker locks.Locker
}

// update is io.update that announces a change: a vacation reply is a script,
// so one event stands for both.
func (s *IndexedStore) update(ctx context.Context, username, homeDir string, fn func(context.Context, *ScriptIndex) (bool, error)) (ScriptIndex, error) {
	changed := false
	ix, err := s.io.update(ctx, username, homeDir, func(ctx context.Context, ix *ScriptIndex) (bool, error) {
		c, ferr := fn(ctx, ix)
		changed = c && ferr == nil
		return c, ferr
	})
	if err == nil && changed {
		locks.EmitBest(s.locker, locks.SieveScriptsKey(username), locks.EventSieve, "")
	}
	return ix, err
}

// Index returns the user's index, giving an id to every stored script that has
// none and dropping the ids of scripts that are gone.
func (s *IndexedStore) Index(ctx context.Context, username, homeDir string) (ScriptIndex, error) {
	return s.update(ctx, username, homeDir, func(ctx context.Context, ix *ScriptIndex) (bool, error) {
		names, err := s.ListScripts(ctx, username, homeDir)
		if err != nil {
			return false, err
		}
		return reconcile(ix, names), nil
	})
}

func reconcile(ix *ScriptIndex, names []string) bool {
	present := make(map[string]bool, len(names))
	changed := false
	for _, n := range names {
		present[n] = true
		if _, ok := ix.IDs[n]; !ok {
			ix.assign(n)
			changed = true
		}
	}
	for n := range ix.IDs {
		if !present[n] {
			delete(ix.IDs, n)
			changed = true
		}
	}
	if changed {
		ix.State++
	}
	return changed
}

// change runs op and then fn on the index, both under the index lock.
func (s *IndexedStore) change(ctx context.Context, username, homeDir string, op func(context.Context) error, fn func(*ScriptIndex)) error {
	_, err := s.update(ctx, username, homeDir, func(ctx context.Context, ix *ScriptIndex) (bool, error) {
		if err := op(ctx); err != nil {
			return false, err
		}
		fn(ix)
		ix.State++
		return true, nil
	})
	return err
}

func (s *IndexedStore) SaveScript(ctx context.Context, username, homeDir, name string, src []byte) error {
	return s.change(ctx, username, homeDir, func(ctx context.Context) error {
		return s.ScriptStore.SaveScript(ctx, username, homeDir, name, src)
	}, func(ix *ScriptIndex) { ix.assign(name) })
}

// CreateScript stores src as a new script, refusing a stored name under the
// lock that a concurrent create or rename also takes.
func (s *IndexedStore) CreateScript(ctx context.Context, username, homeDir, name string, src []byte) error {
	return s.change(ctx, username, homeDir, func(ctx context.Context) error {
		if _, found, err := s.ScriptStore.GetScript(ctx, username, homeDir, name); err != nil || found {
			if err != nil {
				return err
			}
			return ErrScriptExists
		}
		return s.ScriptStore.SaveScript(ctx, username, homeDir, name, src)
	}, func(ix *ScriptIndex) { ix.assign(name) })
}

func (s *IndexedStore) DeleteScript(ctx context.Context, username, homeDir, name string) error {
	return s.change(ctx, username, homeDir, func(ctx context.Context) error {
		return s.ScriptStore.DeleteScript(ctx, username, homeDir, name)
	}, func(ix *ScriptIndex) { delete(ix.IDs, name) })
}

func (s *IndexedStore) RenameScript(ctx context.Context, username, homeDir, oldName, newName string) error {
	return s.change(ctx, username, homeDir, func(ctx context.Context) error {
		return s.ScriptStore.RenameScript(ctx, username, homeDir, oldName, newName)
	}, func(ix *ScriptIndex) {
		if id, ok := ix.IDs[oldName]; ok {
			ix.IDs[newName] = id
			delete(ix.IDs, oldName)
		}
		ix.assign(newName)
	})
}

func (s *IndexedStore) SetActive(ctx context.Context, username, homeDir, name string) error {
	return s.change(ctx, username, homeDir, func(ctx context.Context) error {
		return s.ScriptStore.SetActive(ctx, username, homeDir, name)
	}, func(*ScriptIndex) {})
}

func (s *IndexedStore) Deactivate(ctx context.Context, username, homeDir string) error {
	return s.change(ctx, username, homeDir, func(ctx context.Context) error {
		return s.ScriptStore.Deactivate(ctx, username, homeDir)
	}, func(*ScriptIndex) {})
}

func decodeIndex(raw []byte) (ScriptIndex, error) {
	ix := ScriptIndex{IDs: map[string]string{}}
	if len(raw) == 0 {
		return ix, nil
	}
	if err := json.Unmarshal(raw, &ix); err != nil {
		return ScriptIndex{}, fmt.Errorf("sieve/index: decode: %w", err)
	}
	if ix.IDs == nil {
		ix.IDs = map[string]string{}
	}
	return ix, nil
}

// indexFile is the fs index; the name does not end in ".sieve", so
// ListScripts never reports it.
const indexFile = ".yarilo.sieve-index"

type fsIndex struct{ locker locks.Locker }

func (f fsIndex) update(ctx context.Context, username, homeDir string, fn func(context.Context, *ScriptIndex) (bool, error)) (ScriptIndex, error) {
	var out ScriptIndex
	path := filepath.Join(homeDir, indexFile)
	err := withSieveLock(ctx, f.locker, username, "sieve-index:"+homeDir, func(ctx context.Context) error {
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("sieve/index: read: %w", err)
		}
		ix, err := decodeIndex(raw)
		if err != nil {
			return err
		}
		changed, err := fn(ctx, &ix)
		if err != nil {
			return err
		}
		if changed {
			b, _ := json.Marshal(ix)
			if err := os.MkdirAll(homeDir, 0o700); err != nil {
				return fmt.Errorf("sieve/index: mkdir: %w", err)
			}
			if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
				return fmt.Errorf("sieve/index: write: %w", err)
			}
			if err := os.Rename(path+".tmp", path); err != nil {
				return fmt.Errorf("sieve/index: write: %w", err)
			}
		}
		out = ix
		return nil
	})
	return out, err
}

type dictIndex struct {
	locker locks.Locker
	d      dict.Dict
}

func (di dictIndex) update(ctx context.Context, username, _ string, fn func(context.Context, *ScriptIndex) (bool, error)) (ScriptIndex, error) {
	var out ScriptIndex
	key := dict.PathPrivate + dict.Escape(username) + "/sieve/index"
	op := &dict.OpSettings{Username: username}
	err := withSieveLock(ctx, di.locker, username, "sieve-index:"+username, func(ctx context.Context) error {
		vals, found, err := di.d.Lookup(ctx, op, key)
		if err != nil {
			return fmt.Errorf("sieve/index: lookup: %w", err)
		}
		var raw []byte
		if found {
			raw = vals[0]
		}
		ix, err := decodeIndex(raw)
		if err != nil {
			return err
		}
		changed, err := fn(ctx, &ix)
		if err != nil {
			return err
		}
		if changed {
			b, _ := json.Marshal(ix)
			tx, err := di.d.Begin(ctx, op)
			if err != nil {
				return fmt.Errorf("sieve/index: begin: %w", err)
			}
			if err := tx.Set(key, b); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("sieve/index: set: %w", err)
			}
			if res, err := tx.Commit(); err != nil || res != dict.CommitOK {
				return fmt.Errorf("sieve/index: commit: %w", commitErr(res, err))
			}
		}
		out = ix
		return nil
	})
	return out, err
}
