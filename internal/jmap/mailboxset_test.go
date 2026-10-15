package jmap

import (
	"fmt"
	"testing"
)

func mailboxSetCall(t *testing.T, s *Server, args string) map[string]any {
	t.Helper()
	name, out := callAs(t, s, testUser, "Mailbox/set", args)
	if name != "Mailbox/set" {
		t.Fatalf("answered %s %v", name, out)
	}
	return out
}

func setErrType(out map[string]any, bucket, key string) any {
	m, _ := out[bucket].(map[string]any)
	e, _ := m[key].(map[string]any)
	return e["type"]
}

func TestMailboxSetCreates(t *testing.T) {
	s := storedServer(t)
	ids := mailboxIDs(t, s)
	out := mailboxSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"create":{
		"top":{"name":"New","isSubscribed":true},
		"child":{"name":"Sub","parentId":%q},
		"taken":{"name":"Archive"},
		"deep":{"name":"a/b"},
		"orphan":{"name":"X","parentId":"ffffffffffffffffffffffffffffffff"},
		"role":{"name":"R","role":"trash"}}}`, testUser, ids["Work"]))
	created, _ := out["created"].(map[string]any)
	if created["top"] == nil || created["child"] == nil {
		t.Fatalf("not created: %v", out["notCreated"])
	}
	for key, want := range map[string]string{"taken": "alreadyExists", "deep": "invalidProperties", "orphan": "invalidProperties", "role": "invalidProperties"} {
		if got := setErrType(out, "notCreated", key); got != want {
			t.Errorf("%s answered %v, want %s", key, got, want)
		}
	}
	after := mailboxIDs(t, s)
	if after["New"] == "" || parentOf(t, s, after["Sub"]) != ids["Work"] {
		t.Errorf("mailboxes after create: %v", after)
	}
	_, got := callAs(t, s, testUser, "Mailbox/get", fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["isSubscribed"]}`, testUser, after["New"]))
	if fmt.Sprint(got["list"]) != fmt.Sprintf("[map[id:%s isSubscribed:true]]", after["New"]) {
		t.Errorf("New subscribed: %v", got["list"])
	}
}

// A rename keeps the mailbox id: it is the folder's GUID.
func TestMailboxSetRenamesAndMoves(t *testing.T) {
	s := storedServer(t)
	ids := mailboxIDs(t, s)
	out := mailboxSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{
		%q:{"name":"Old"},
		%q:{"parentId":null},
		%q:{"name":"Mail"}}}`, testUser, ids["Archive"], ids["Reports"], ids["INBOX"]))
	after := mailboxIDs(t, s)
	if after["Old"] != ids["Archive"] || after["Reports"] != ids["Reports"] || parentOf(t, s, ids["Reports"]) != "" {
		t.Errorf("after rename: %v (was %v)", after, ids)
	}
	if got := setErrType(out, "notUpdated", ids["INBOX"]); got != "forbidden" {
		t.Errorf("INBOX rename answered %v", got)
	}
}

func TestMailboxSetDestroys(t *testing.T) {
	s := storedServer(t)
	ids := mailboxIDs(t, s)
	id := inboxEmails(t, s)[0]
	emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:{"mailboxIds/%s":true}}}`, testUser, id, ids["Archive"]))

	out := mailboxSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"destroy":[%q,%q,%q,%q]}`,
		testUser, ids["INBOX"], ids["Work"], ids["Archive"], ids["Sent"]))
	for key, want := range map[string]string{"INBOX": "forbidden", "Work": "mailboxHasChild", "Archive": "mailboxHasEmail"} {
		if got := setErrType(out, "notDestroyed", ids[key]); got != want {
			t.Errorf("destroy %s answered %v, want %s", key, got, want)
		}
	}
	out = mailboxSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"destroy":[%q],"onDestroyRemoveEmails":true}`, testUser, ids["Archive"]))
	if d, _ := out["destroyed"].([]any); len(d) != 1 {
		t.Fatalf("Archive with onDestroyRemoveEmails: %v", out)
	}
	after := mailboxIDs(t, s)
	if after["Archive"] != "" || after["Sent"] != "" {
		t.Errorf("mailboxes after destroy: %v", after)
	}
	if got := where(t, s, id); fmt.Sprint(got) != "[INBOX]" {
		t.Errorf("the Email kept its other copy? it is in %v", got)
	}
}

// parentOf is a mailbox's parentId, "" at the top level.
func parentOf(t *testing.T, s *Server, id string) string {
	t.Helper()
	_, out := callAs(t, s, testUser, "Mailbox/get", fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["parentId"]}`, testUser, id))
	list, _ := out["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("no mailbox %s: %v", id, out)
	}
	p, _ := list[0].(map[string]any)["parentId"].(string)
	return p
}
