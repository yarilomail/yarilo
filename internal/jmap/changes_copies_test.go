package jmap

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A move, a copy or a removed copy is an update of the one Email; only the last
// copy going is destroyed, and an id born and gone in the window is in no list.
func TestEmailChangesJudgesTheEmailNotTheCopy(t *testing.T) {
	set := func(t *testing.T, s *Server, args string) {
		t.Helper()
		resp := emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,%s}`, testAccount, args))
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
			resp := emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"create":{"c1":{"mailboxIds":{%q:true},"subject":"brief","textBody":[{"partId":"t"}],"bodyValues":{"t":{"value":"x"}}}}}`, testAccount, mb["INBOX"]))
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
			got, errType := changesCall(t, s, "Email/changes", fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, since))
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

// rebuildGUIDStore rebuilds the store from every folder, as the admin API does.
func rebuildGUIDStore(t *testing.T, s *Server) {
	t.Helper()
	h, err := s.opts.Storage.open(testUser, "")
	if err != nil {
		t.Fatal(err)
	}
	marks, err := s.folderMarks(h)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uint64, 0, len(marks))
	for _, f := range marks {
		ids = append(ids, f.folder.ID)
	}
	if _, err := h.index().(mailbox.GUIDStoreRebuilder).RebuildGUIDStore(ids); err != nil {
		t.Fatal(err)
	}
}

// The last copy going is the common change: a store marked whole answers it
// without opening a folder; one that may lack older mail is walked.
func TestEmailChangesWalksOnlyAStoreNotMarkedWhole(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rebuilt bool
	}{{"store rebuilt", true}, {"store never rebuilt", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := storedServer(t)
			id := inboxEmails(t, s)[0]
			if tc.rebuilt {
				rebuildGUIDStore(t, s)
			}
			since := emailStateOf(t, s)
			emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"destroy":[%q]}`, testAccount, id))
			before := testutil.ToFloat64(metricIDLookupFolders)
			got, errType := changesCall(t, s, "Email/changes", fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, since))
			walked := testutil.ToFloat64(metricIDLookupFolders) - before
			if errType != "" {
				t.Fatalf("Email/changes answered %s", errType)
			}
			if d := changedIDsOf(t, got, "destroyed"); !slices.Equal(d, []string{id}) {
				t.Errorf("destroyed %v, want [%s]", d, id)
			}
			if tc.rebuilt && walked != 0 {
				t.Errorf("a store marked whole still opened %v folders", walked)
			}
			if !tc.rebuilt && walked == 0 {
				t.Error("a store that may lack older mail was trusted to say the Email is gone")
			}
		})
	}
}

// One id the store names must not make the store answer for the others: on a
// store that is not whole, an older copy it never recorded is still alive.
func TestEmailChangesTrustsNoBatchOnAStoreNotWhole(t *testing.T) {
	s := storedServer(t)
	ids, mb := inboxEmails(t, s), mailboxIDs(t, s)
	if len(ids) < 2 {
		t.Fatalf("the fixture holds %d emails, the row needs two", len(ids))
	}
	older, newer := ids[0], ids[1]
	emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:{"mailboxIds/%s":true}}}`, testAccount, older, mb["Work"]))
	// Mail from before the store: neither copy of the older Email is in it.
	info, err := s.opts.Storage.ResolveUser(testUser)
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(info.Home, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), file.GUIDIndexFileName) {
			return os.Remove(p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	since := emailStateOf(t, s)
	// The store comes back with the newer Email's copy only.
	emailSetCall(t, s, fmt.Sprintf(`{"accountId":%q,"update":{%q:{"mailboxIds/%s":true},%q:{"mailboxIds/%s":null}}}`,
		testAccount, newer, mb["Archive"], older, mb["Work"]))
	got, errType := changesCall(t, s, "Email/changes", fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, since))
	if errType != "" {
		t.Fatalf("Email/changes answered %s", errType)
	}
	if d := changedIDsOf(t, got, "destroyed"); slices.Contains(d, older) {
		t.Errorf("the older Email, alive in INBOX, is reported destroyed: %v", got)
	}
	if u := changedIDsOf(t, got, "updated"); !slices.Contains(u, older) || !slices.Contains(u, newer) {
		t.Errorf("updated %v, want both %s and %s", u, older, newer)
	}
}
