package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const liveUser = "alice@example.com"

// decode reads a command's JSON output into v.
func decode(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
}

func folderNames(t *testing.T) string {
	t.Helper()
	var resp struct {
		Folders []string `json:"folders"`
	}
	decode(t, mustYarctl(t, "backend", "folder", "list", liveUser), &resp)
	return strings.Join(resp.Folders, ",")
}

// Folder verbs create, describe, rename and delete folders in the user's store.
func TestFolderVerbsAgainstTheBackend(t *testing.T) {
	startLiveBackend(t)
	mustYarctl(t, "backend", "folder", "create", liveUser, "Work")
	mustYarctl(t, "backend", "folder", "create", liveUser, "Sent", "--special-use", `\Sent`)
	if got := folderNames(t); got != "INBOX,Sent,Work" {
		t.Errorf("after two creates the folders are %s", got)
	}

	var info struct {
		Name     string `json:"name"`
		Created  bool   `json:"created"`
		GUID     string `json:"guid"`
		Messages int    `json:"messages"`
	}
	decode(t, mustYarctl(t, "backend", "folder", "info", liveUser, "Work"), &info)
	if info.Name != "Work" || !info.Created || len(info.GUID) != 32 || info.Messages != 0 {
		t.Errorf("info of Work: %+v", info)
	}
	var guid struct {
		GUID string `json:"guid"`
	}
	decode(t, mustYarctl(t, "backend", "folder", "guid", liveUser, "Work"), &guid)
	if guid.GUID != info.GUID {
		t.Errorf("guid %s differs from info's %s", guid.GUID, info.GUID)
	}

	mustYarctl(t, "backend", "folder", "rename", liveUser, "Work", "Projects")
	if got := folderNames(t); got != "INBOX,Projects,Sent" {
		t.Errorf("after the rename the folders are %s", got)
	}
	decode(t, mustYarctl(t, "backend", "folder", "guid", liveUser, "Projects"), &guid)
	if guid.GUID != info.GUID {
		t.Errorf("the rename changed the GUID: %s, was %s", guid.GUID, info.GUID)
	}

	mustYarctl(t, "backend", "folder", "delete", liveUser, "Projects")
	if got := folderNames(t); got != "INBOX,Sent" {
		t.Errorf("after the delete the folders are %s", got)
	}
	if _, err := yarctl(t, "backend", "folder", "info", liveUser, "Projects"); err == nil {
		t.Error("info of a deleted folder succeeded")
	}
}

// Subscriptions, special-use, ACL and annotations written through yarctl are
// what the next read returns.
func TestUserStateVerbsAgainstTheBackend(t *testing.T) {
	startLiveBackend(t)
	mustYarctl(t, "backend", "folder", "create", liveUser, "Projects")
	mustYarctl(t, "backend", "folder", "create", liveUser, "Sent", "--special-use", `\Sent`)

	mustYarctl(t, "backend", "subs", "add", liveUser, "Projects")
	var subs struct {
		Subscriptions []string `json:"subscriptions"`
	}
	decode(t, mustYarctl(t, "backend", "subs", "list", liveUser), &subs)
	if strings.Join(subs.Subscriptions, ",") != "Projects" {
		t.Errorf("subscriptions after add: %v", subs.Subscriptions)
	}
	mustYarctl(t, "backend", "subs", "remove", liveUser, "Projects")
	decode(t, mustYarctl(t, "backend", "subs", "list", liveUser), &subs)
	if len(subs.Subscriptions) != 0 {
		t.Errorf("subscriptions after remove: %v", subs.Subscriptions)
	}

	var su struct {
		Attr   string `json:"attr"`
		Source string `json:"source"`
	}
	decode(t, mustYarctl(t, "backend", "specialuse", "get", liveUser, "Sent"), &su)
	if su.Attr != `\Sent` || su.Source != "override" {
		t.Errorf("special-use of Sent: %+v", su)
	}
	mustYarctl(t, "backend", "specialuse", "set", liveUser, "Projects", `\Archive`)
	decode(t, mustYarctl(t, "backend", "specialuse", "get", liveUser, "Projects"), &su)
	if su.Attr != `\Archive` {
		t.Errorf("special-use of Projects after set: %+v", su)
	}
	mustYarctl(t, "backend", "specialuse", "delete", liveUser, "Projects")
	decode(t, mustYarctl(t, "backend", "specialuse", "get", liveUser, "Projects"), &su)
	if su.Attr != "" {
		t.Errorf("special-use of Projects after delete: %+v", su)
	}

	mustYarctl(t, "backend", "acl", "set", liveUser, "Projects", "bob@example.com", "lr")
	var acl struct {
		ACL []struct{ Identifier, Rights string } `json:"acl"`
	}
	decode(t, mustYarctl(t, "backend", "acl", "get", liveUser, "Projects"), &acl)
	if len(acl.ACL) != 1 || acl.ACL[0].Identifier != "user=bob@example.com" || acl.ACL[0].Rights != "lr" {
		t.Errorf("ACL after set: %+v", acl.ACL)
	}
	mustYarctl(t, "backend", "acl", "delete", liveUser, "Projects", "bob@example.com")
	decode(t, mustYarctl(t, "backend", "acl", "get", liveUser, "Projects"), &acl)
	if len(acl.ACL) != 0 {
		t.Errorf("ACL after delete: %+v", acl.ACL)
	}

	mustYarctl(t, "backend", "metadata", "set", liveUser, "Projects", "--entry", "/private/comment", "--value", "hello")
	var md struct {
		Found bool   `json:"found"`
		Value []byte `json:"value"`
	}
	decode(t, mustYarctl(t, "backend", "metadata", "get", liveUser, "Projects", "--entry", "/private/comment"), &md)
	if !md.Found || string(md.Value) != "hello" {
		t.Errorf("annotation after set: %+v", md)
	}
	mustYarctl(t, "backend", "metadata", "delete", liveUser, "Projects", "--entry", "/private/comment")
	decode(t, mustYarctl(t, "backend", "metadata", "get", liveUser, "Projects", "--entry", "/private/comment"), &md)
	if md.Found {
		t.Errorf("annotation after delete: %+v", md)
	}
}

// user info and usage describe the user the server resolves.
func TestUserVerbsAgainstTheBackend(t *testing.T) {
	b := startLiveBackend(t)
	mustYarctl(t, "backend", "folder", "create", liveUser, "Projects")
	info := mustYarctl(t, "backend", "user", "info", liveUser)
	if !strings.Contains(info, "Username:        "+liveUser) || !strings.Contains(info, b.root) {
		t.Errorf("user info:\n%s", info)
	}
	usage := mustYarctl(t, "backend", "user", "usage", liveUser)
	for _, want := range []string{"INBOX", "Projects", "Total"} {
		if !strings.Contains(usage, want) {
			t.Errorf("user usage has no %s:\n%s", want, usage)
		}
	}
}
