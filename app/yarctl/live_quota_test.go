package main

import (
	"context"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/backendapi"
	"github.com/yarilomail/yarilo/v2/pkg/dict"
	"github.com/yarilomail/yarilo/v2/pkg/dict/memory"
	"github.com/yarilomail/yarilo/v2/pkg/quota"
)

// Usage is what the store holds, a recalc says the same in both output
// formats, and the clone reader shows a mirror as it is: found, missing or
// malformed, and only for a configured clone.
func TestQuotaVerbsAgainstTheBackend(t *testing.T) {
	clone, err := memory.New(dict.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clone.Close() })
	b := startLiveBackendWith(t, func(o *backendapi.Options) {
		o.Dicts["mirror"] = clone
		o.QuotaCloneDicts = []string{"mirror"}
	})
	seedInbox(t, b, map[string]string{
		"1700000000.M1P1.host:2,S": liveMsg1,
		"1700000001.M2P1.host:2,":  liveMsg2,
	})

	var show struct {
		MessageValue int64 `json:"message_value"`
		StorageValue int64 `json:"storage_value"`
	}
	outputFormat = "json"
	decode(t, mustYarctl(t, "backend", "quota", "show", liveUser), &show)
	if show.MessageValue != 2 {
		t.Errorf("quota show: %+v, want 2 messages", show)
	}
	var recalc struct {
		Messages     int64 `json:"messages"`
		StorageBytes int64 `json:"storage_bytes"`
	}
	decode(t, mustYarctl(t, "backend", "quota", "recalc", liveUser), &recalc)
	if recalc.Messages != 2 || recalc.StorageBytes != int64(len(liveMsg1)+len(liveMsg2)) {
		t.Errorf("quota recalc: %+v, want 2 messages, %d bytes", recalc, len(liveMsg1)+len(liveMsg2))
	}

	outputFormat = "human"
	if out := mustYarctl(t, "backend", "quota", "show", liveUser); !strings.Contains(out, "MESSAGE") || !strings.Contains(out, " 2 ") {
		t.Errorf("human quota show:\n%s", out)
	}
	if out := mustYarctl(t, "backend", "quota", "recalc", liveUser); !strings.Contains(out, "Recalculated "+liveUser+": 2 messages") {
		t.Errorf("human quota recalc: %q", out)
	}

	outputFormat = "json"
	ctx := context.Background()
	tx, err := clone.Begin(ctx, &dict.OpSettings{Username: liveUser})
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Set(quota.KeyStorage, []byte("not a number"))
	_ = tx.Set(quota.KeyMessages, []byte("2"))
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Backends []string `json:"backends"`
	}
	decode(t, mustYarctl(t, "backend", "quota", "clone", "list"), &list)
	if len(list.Backends) != 1 || list.Backends[0] != "mirror" {
		t.Errorf("clone list: %v, want mirror", list.Backends)
	}
	var got struct {
		Messages      int64    `json:"messages"`
		MessagesFound bool     `json:"messages_found"`
		StorageFound  bool     `json:"storage_found"`
		Malformed     []string `json:"malformed"`
	}
	decode(t, mustYarctl(t, "backend", "quota", "clone", "get", "mirror", liveUser), &got)
	if got.Messages != 2 || !got.MessagesFound || !got.StorageFound || len(got.Malformed) != 1 || got.Malformed[0] != quota.KeyStorage {
		t.Errorf("clone get: %+v, want 2 messages and the storage value flagged malformed", got)
	}
	if _, err := yarctl(t, "backend", "quota", "clone", "get", "elsewhere", liveUser); err == nil {
		t.Error("a backend that is not a configured clone was read")
	}

	for _, args := range [][]string{
		{"backend", "quota", "show"},
		{"backend", "quota", "recalc"},
		{"backend", "quota", "clone", "get", "mirror"},
	} {
		if _, err := yarctl(t, args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("yarctl %s = %v, want its usage", strings.Join(args, " "), err)
		}
	}
	for _, args := range [][]string{{"backend", "quota", "nosuch"}, {"backend", "quota", "clone", "nosuch"}} {
		if _, err := yarctl(t, args...); err == nil {
			t.Errorf("yarctl %s was accepted", strings.Join(args, " "))
		}
	}
}
