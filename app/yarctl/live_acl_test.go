package main

import (
	"context"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/dict"
)

type liveACL struct {
	ACL []struct{ Identifier, Rights string } `json:"acl"`
}

// The namespace root's ACL is set, read and dropped like a folder's.
func TestACLRootVerbs(t *testing.T) {
	startLiveBackend(t)
	mustYarctl(t, "backend", "folder", "create", liveUser, "Projects")
	mustYarctl(t, "backend", "acl", "set", "--root", liveUser, "bob@example.com", "l")
	var got liveACL
	decode(t, mustYarctl(t, "backend", "acl", "get", "--root", liveUser), &got)
	if len(got.ACL) != 1 || got.ACL[0].Identifier != "user=bob@example.com" || got.ACL[0].Rights != "l" {
		t.Errorf("root ACL after set: %+v", got.ACL)
	}
	mustYarctl(t, "backend", "acl", "delete", "--root", liveUser)
	decode(t, mustYarctl(t, "backend", "acl", "get", "--root", liveUser), &got)
	if len(got.ACL) != 0 {
		t.Errorf("root ACL after delete: %+v", got.ACL)
	}
}

// The index verbs report what the folders hold: a dry run finds no drift on
// an index written through set, and --all rebuilds exactly the folders with an ACL.
func TestACLIndexVerbs(t *testing.T) {
	startLiveBackend(t)
	mustYarctl(t, "backend", "folder", "create", liveUser, "Projects")
	mustYarctl(t, "backend", "folder", "create", liveUser, "Projects/Sub")
	mustYarctl(t, "backend", "acl", "set", liveUser, "Projects", "bob@example.com", "lr")

	var list struct {
		Entries []struct{ Mailbox, Identifier, Rights string } `json:"entries"`
	}
	decode(t, mustYarctl(t, "backend", "acl", "list", liveUser), &list)
	if len(list.Entries) != 1 || list.Entries[0].Mailbox != "Projects" {
		t.Errorf("acl list: %+v", list.Entries)
	}
	var dry struct {
		DryRun bool `json:"dry_run"`
		InSync bool `json:"in_sync"`
	}
	decode(t, mustYarctl(t, "backend", "acl", "rebuild", liveUser, "Projects", "--dry-run"), &dry)
	if !dry.DryRun || !dry.InSync {
		t.Errorf("dry run: %+v", dry)
	}
	var all struct {
		Rebuilt []string `json:"rebuilt"`
		Skipped []struct{ Folder, Reason string }
	}
	decode(t, mustYarctl(t, "backend", "acl", "rebuild", liveUser, "--all"), &all)
	if strings.Join(all.Rebuilt, ",") != "Projects" || len(all.Skipped) != 2 {
		t.Errorf("rebuild --all: %+v", all)
	}
	var mat struct {
		Applied bool `json:"applied"`
	}
	decode(t, mustYarctl(t, "backend", "acl", "materialise", liveUser, "Projects/Sub"), &mat)
	if mat.Applied {
		t.Error("materialise without --apply applied")
	}
	decode(t, mustYarctl(t, "backend", "acl", "materialise", liveUser, "Projects/Sub", "--apply"), &mat)
	if !mat.Applied {
		t.Error("materialise --apply did not apply")
	}
	if _, err := yarctl(t, "backend", "acl", "rebuild", liveUser); err == nil {
		t.Error("rebuild with neither folders nor --all succeeded")
	}
}

// registry list names the owners the registry holds for the user.
func TestACLRegistryList(t *testing.T) {
	b := startLiveBackend(t)
	tx, err := b.shared.Begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Set("shared/shared-boxes/user/"+dict.Escape("bob@example.com")+"/"+liveUser, []byte("1"))
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Owners []string `json:"owners"`
	}
	decode(t, mustYarctl(t, "backend", "acl", "registry", "list", "bob@example.com"), &reg)
	if strings.Join(reg.Owners, ",") != liveUser {
		t.Errorf("registry list: %v, want %s", reg.Owners, liveUser)
	}
	mustYarctl(t, "backend", "acl", "registry", "rebuild", liveUser, "--namespace", "personal")
}

// The dict verbs that answer about the server rather than a key.
func TestDictServerVerbs(t *testing.T) {
	startLiveBackend(t)
	if out := mustYarctl(t, "backend", "dict", "drivers"); !strings.Contains(out, "memory") {
		t.Errorf("drivers: %s", out)
	}
	if out := mustYarctl(t, "backend", "dict", "expire-scan", "shared"); !strings.Contains(out, "ok") {
		t.Errorf("expire-scan: %s", out)
	}
}
