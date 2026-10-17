package sieve

import (
	"context"
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/memory"
)

func quotaStores(t *testing.T) map[string]ScriptStore {
	t.Helper()
	d, err := memory.New(dict.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]ScriptStore{
		"fs":   NewScriptStore("fs", "", nil, nil),
		"dict": NewScriptStore("redis", "", nil, d),
	}
}

func TestCheckQuota(t *testing.T) {
	cases := []struct {
		name     string
		stored   []string
		script   string
		size     int
		q        Quota
		wantKind QuotaKind // 0 = within quota
		wantLim  int
	}{
		{"no limits", []string{"a", "b", "c"}, "d", 1 << 20, Quota{}, 0, 0},
		{"a new name under the count", []string{"a"}, "b", 10, Quota{MaxScripts: 2}, 0, 0},
		{"a new name past the count", []string{"a", "b"}, "c", 10, Quota{MaxScripts: 2}, QuotaMaxScripts, 2},
		{"a replaced name at the count", []string{"a", "b"}, "b", 10, Quota{MaxScripts: 2}, 0, 0},
		{"the first script against a limit of one", nil, "a", 10, Quota{MaxScripts: 1}, 0, 0},
		{"exactly the size limit", nil, "a", 100, Quota{MaxSize: 100}, 0, 0},
		{"one byte past the size limit", nil, "a", 101, Quota{MaxSize: 100}, QuotaMaxSize, 100},
		{"size is judged before count", []string{"a"}, "b", 101, Quota{MaxSize: 100, MaxScripts: 1}, QuotaMaxSize, 100},
	}
	for driver := range quotaStores(t) {
		for _, c := range cases {
			t.Run(driver+"/"+c.name, func(t *testing.T) {
				ctx := context.Background()
				store := quotaStores(t)[driver]
				home := t.TempDir()
				for _, n := range c.stored {
					if err := store.SaveScript(ctx, "u", home, n, []byte("keep;\n")); err != nil {
						t.Fatal(err)
					}
				}
				err := CheckQuota(ctx, store, "u", home, c.script, c.size, c.q)
				var qe *QuotaExceededError
				switch {
				case c.wantKind == 0 && err != nil:
					t.Fatalf("CheckQuota = %v, want within quota", err)
				case c.wantKind != 0 && !errors.As(err, &qe):
					t.Fatalf("CheckQuota = %v, want a quota refusal", err)
				case c.wantKind != 0 && (qe.Kind != c.wantKind || qe.Limit != c.wantLim):
					t.Errorf("refusal = kind %d limit %d, want kind %d limit %d", qe.Kind, qe.Limit, c.wantKind, c.wantLim)
				}
			})
		}
	}
}
