package jmap

import (
	"fmt"
	"sort"
	"testing"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// mailboxIDs maps folder name to mailbox id for the caller.
func mailboxIDs(t *testing.T, s *Server) map[string]string {
	t.Helper()
	_, out := callAs(t, s, testUser, "Mailbox/get", `{"accountId":"`+testUser+`"}`)
	ids := map[string]string{}
	for _, mb := range out["list"].([]any) {
		m := mb.(map[string]any)
		ids[m["name"].(string)] = m["id"].(string)
	}
	return ids
}

func inboxEmails(t *testing.T, s *Server) []string {
	t.Helper()
	_, out := callAs(t, s, testUser, "Email/query", fmt.Sprintf(`{"accountId":%q,"filter":{"inMailbox":%q}}`, testUser, mailboxIDs(t, s)["INBOX"]))
	var ids []string
	for _, id := range out["ids"].([]any) {
		ids = append(ids, id.(string))
	}
	return ids
}

// where answers the names of the mailboxes an Email is in.
func where(t *testing.T, s *Server, id string) []string {
	t.Helper()
	byID := map[string]string{}
	for name, mid := range mailboxIDs(t, s) {
		byID[mid] = name
	}
	got := emailGet(t, s, fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["mailboxIds"]}`, testUser, id))
	list, _ := got["list"].([]any)
	if len(list) == 0 {
		return nil
	}
	var names []string
	for mid := range list[0].(map[string]any)["mailboxIds"].(map[string]any) {
		names = append(names, byID[mid])
	}
	sort.Strings(names)
	return names
}

func TestEmailSetMovesCopiesAndRemoves(t *testing.T) {
	cases := []struct {
		name  string
		patch func(ids map[string]string) string
		want  []string
		err   string
	}{
		{"move", func(m map[string]string) string { return fmt.Sprintf(`{"mailboxIds":{%q:true}}`, m["Archive"]) }, []string{"Archive"}, ""},
		{"copy", func(m map[string]string) string { return fmt.Sprintf(`{"mailboxIds/%s":true}`, m["Work"]) }, []string{"INBOX", "Work"}, ""},
		{"patch out of the only mailbox", func(m map[string]string) string { return fmt.Sprintf(`{"mailboxIds/%s":null}`, m["INBOX"]) }, []string{"INBOX"}, jmapcore.SetErrInvalidProps},
		{"both forms", func(m map[string]string) string {
			return fmt.Sprintf(`{"mailboxIds":{%q:true},"mailboxIds/%s":true}`, m["Work"], m["Sent"])
		}, []string{"INBOX"}, jmapcore.SetErrInvalidPatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := storedServer(t)
			id := inboxEmails(t, s)[0]
			resp := emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:%s}}`, testUser, id, tc.patch(mailboxIDs(t, s))))
			if tc.err != "" {
				if e := resp.NotUpdated[id]; e == nil || e.Type != tc.err {
					t.Fatalf("answered %+v, want %s", resp, tc.err)
				}
			} else if _, ok := resp.Updated[id]; !ok {
				t.Fatalf("not updated: %+v", resp.NotUpdated[id])
			}
			// The id is the GUID and does not change (RFC 8474).
			if got := where(t, s, id); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("the Email is in %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEmailSetDestroysEveryCopy(t *testing.T) {
	s := storedServer(t)
	id := inboxEmails(t, s)[0]
	emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:{"mailboxIds/%s":true}}}`, testUser, id, mailboxIDs(t, s)["Work"]))
	resp := emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"destroy":[%q,"nosuchid"]}`, testUser, id))
	if len(resp.Destroyed) != 1 || resp.Destroyed[0] != id {
		t.Fatalf("destroyed %v: %+v", resp.Destroyed, resp.NotDestroyed)
	}
	if e := resp.NotDestroyed["nosuchid"]; e == nil || e.Type != jmapcore.SetErrNotFound {
		t.Errorf("unknown id answered %+v", e)
	}
	if got := where(t, s, id); len(got) != 0 {
		t.Errorf("a destroyed Email is still in %v", got)
	}
}

func TestEmailSetHoldsMaxObjectsInSet(t *testing.T) {
	s := storedServer(t)
	s.opts.Limits.MaxObjectsInSet = 1
	ids := inboxEmails(t, s)
	name, out := callAs(t, s, testUser, "Email/set", fmt.Sprintf(`{"accountId":%q,"destroy":[%q,%q]}`, testUser, ids[0], ids[1]))
	wantMethodError(t, name, out, "requestTooLarge")
	if len(inboxEmails(t, s)) != 2 {
		t.Error("a refused call destroyed mail")
	}
}
