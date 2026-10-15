package jmap

import (
	"fmt"
	"slices"
	"testing"
)

// A move, a copy or a removed copy is an update of the one Email; only the last
// copy going is destroyed, and an id born and gone in the window is in no list.
func TestEmailChangesJudgesTheEmailNotTheCopy(t *testing.T) {
	set := func(t *testing.T, s *Server, args string) {
		t.Helper()
		resp := emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,%s}`, testUser, args))
		if len(resp.NotCreated)+len(resp.NotUpdated)+len(resp.NotDestroyed) > 0 {
			t.Fatalf("Email/set refused: %+v %+v %+v", resp.NotCreated, resp.NotUpdated, resp.NotDestroyed)
		}
	}
	cases := []struct {
		name   string
		before func(t *testing.T, s *Server, id string, mb map[string]string)
		act    func(t *testing.T, s *Server, id string, mb map[string]string) string
		want   string // the one list holding the id, "" for none
	}{
		{"move", nil, func(t *testing.T, s *Server, id string, mb map[string]string) string {
			set(t, s, fmt.Sprintf(`"update":{%q:{"mailboxIds":{%q:true}}}`, id, mb["Archive"]))
			return id
		}, "updated"},
		{"copy into a second mailbox", nil, func(t *testing.T, s *Server, id string, mb map[string]string) string {
			set(t, s, fmt.Sprintf(`"update":{%q:{"mailboxIds/%s":true}}`, id, mb["Work"]))
			return id
		}, "updated"},
		{"one of two copies removed", func(t *testing.T, s *Server, id string, mb map[string]string) {
			set(t, s, fmt.Sprintf(`"update":{%q:{"mailboxIds/%s":true}}`, id, mb["Work"]))
		}, func(t *testing.T, s *Server, id string, mb map[string]string) string {
			set(t, s, fmt.Sprintf(`"update":{%q:{"mailboxIds/%s":null}}`, id, mb["Work"]))
			return id
		}, "updated"},
		{"last copy destroyed", nil, func(t *testing.T, s *Server, id string, _ map[string]string) string {
			set(t, s, fmt.Sprintf(`"destroy":[%q]`, id))
			return id
		}, "destroyed"},
		{"created and destroyed in the window", nil, func(t *testing.T, s *Server, _ string, mb map[string]string) string {
			resp := emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"create":{"c1":{"mailboxIds":{%q:true},"subject":"brief","textBody":[{"partId":"t"}],"bodyValues":{"t":{"value":"x"}}}}}`, testUser, mb["INBOX"]))
			c1, ok := resp.Created["c1"].(map[string]any)
			if !ok {
				t.Fatalf("not created: %+v", resp.NotCreated)
			}
			born := c1["id"].(string)
			set(t, s, fmt.Sprintf(`"destroy":[%q]`, born))
			return born
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := storedServer(t)
			id, mb := inboxEmails(t, s)[0], mailboxIDs(t, s)
			if tc.before != nil {
				tc.before(t, s, id, mb)
			}
			since := emailStateOf(t, s)
			subject := tc.act(t, s, id, mb)
			got, errType := changesCall(t, s, "Email/changes", fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testUser, since))
			if errType != "" {
				t.Fatalf("Email/changes answered %s", errType)
			}
			var in []string
			for _, list := range []string{"created", "updated", "destroyed"} {
				if slices.Contains(changedIDsOf(t, got, list), subject) {
					in = append(in, list)
				}
			}
			want := []string{tc.want}
			if tc.want == "" {
				want = nil
			}
			if !slices.Equal(in, want) {
				t.Errorf("id in %v, want %v: %v", in, want, got)
			}
		})
	}
}
