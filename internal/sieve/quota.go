package sieve

import (
	"context"
	"fmt"
	"slices"
)

// Quota holds the per-user script limits; a zero field lifts that limit.
type Quota struct {
	MaxSize    int
	MaxScripts int
	MaxStorage int
}

// QuotaKind names the limit a script would exceed.
type QuotaKind int

const (
	QuotaMaxSize QuotaKind = iota + 1
	QuotaMaxScripts
	QuotaMaxStorage
)

// QuotaExceededError reports the limit a save would break and its value.
type QuotaExceededError struct {
	Kind  QuotaKind
	Limit int
}

func (e *QuotaExceededError) Error() string {
	switch e.Kind {
	case QuotaMaxScripts:
		return fmt.Sprintf("script count quota exceeded (max %d scripts)", e.Limit)
	case QuotaMaxStorage:
		return fmt.Sprintf("script storage quota exceeded (max %d bytes)", e.Limit)
	}
	return fmt.Sprintf("script is too large (max %d bytes)", e.Limit)
}

// CheckQuota reports whether saving size bytes as name stays within q: nil, a
// *QuotaExceededError or a storage error; a replaced script is charged once.
func CheckQuota(ctx context.Context, store ScriptStore, username, homeDir, name string, size int, q Quota) error {
	if q.MaxSize > 0 && size > q.MaxSize {
		return &QuotaExceededError{Kind: QuotaMaxSize, Limit: q.MaxSize}
	}
	if q.MaxScripts <= 0 && q.MaxStorage <= 0 {
		return nil
	}
	names, err := store.ListScripts(ctx, username, homeDir)
	if err != nil {
		return fmt.Errorf("sieve/quota: %w", err)
	}
	if q.MaxScripts > 0 && !slices.Contains(names, name) && len(names)+1 > q.MaxScripts {
		return &QuotaExceededError{Kind: QuotaMaxScripts, Limit: q.MaxScripts}
	}
	if q.MaxStorage <= 0 {
		return nil
	}
	total := size
	for _, n := range names {
		if n == name {
			continue
		}
		src, _, err := store.GetScript(ctx, username, homeDir, n)
		if err != nil {
			return fmt.Errorf("sieve/quota: %w", err)
		}
		total += len(src)
	}
	if total > q.MaxStorage {
		return &QuotaExceededError{Kind: QuotaMaxStorage, Limit: q.MaxStorage}
	}
	return nil
}
