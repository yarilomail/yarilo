package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// copyQueryStub answers Mailbox/get from names and Email/query from ids(text,
// inMailbox), so the judgement can be driven without a cluster.
func copyQueryStub(t *testing.T, names map[string]string, ids func(text, inMailbox string) []string) {
	t.Helper()
	stubJMAP(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		_ = json.Unmarshal(raw, &req)
		var method string
		_ = json.Unmarshal(req.MethodCalls[0][0], &method)
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "Mailbox/get":
			list := ""
			for name, id := range names {
				if list != "" {
					list += ","
				}
				list += fmt.Sprintf(`{"id":%q,"name":%q}`, id, name)
			}
			fmt.Fprintf(w, `{"methodResponses":[["Mailbox/get",{"list":[%s]},"c0"]]}`, list) //nolint:errcheck
		case "Email/get":
			var args struct {
				IDs []string `json:"ids"`
			}
			_ = json.Unmarshal(req.MethodCalls[0][1], &args)
			// Only the mailboxes that hold a copy: INBOX and the copy folder,
			// never the untouched one.
			boxes := fmt.Sprintf(`{"inbox":true,%q:true}`, names[copyFolderName])
			fmt.Fprintf(w, `{"methodResponses":[["Email/get",{"list":[{"id":%q,"mailboxIds":%s}]},"c0"]]}`, args.IDs[0], boxes) //nolint:errcheck
		case "Email/query":
			var args struct {
				Filter struct {
					Text      string `json:"text"`
					InMailbox string `json:"inMailbox"`
				} `json:"filter"`
			}
			_ = json.Unmarshal(req.MethodCalls[0][1], &args)
			out, _ := json.Marshal(ids(args.Filter.Text, args.Filter.InMailbox))
			fmt.Fprintf(w, `{"methodResponses":[["Email/query",{"ids":%s,"queryState":"1"},"c0"]]}`, out) //nolint:errcheck
		default:
			fmt.Fprint(w, `{"methodResponses":[["error",{"type":"unknownMethod"},"c0"]]}`) //nolint:errcheck
		}
	})
}

const (
	copyFolderName  = "SmokeFTSCopy1"
	otherFolderName = "SmokeFTSOther1"
)

var copyFolderIDs = map[string]string{copyFolderName: "mb-copy", otherFolderName: "mb-other"}

// Both mutations fail here: a document per copy (two ids for one message) and
// a lookup that ignores the folder terms (the untouched folder answers).
func TestOneEmailIDForCopies(t *testing.T) {
	tests := []struct {
		name    string
		ids     func(text, inMailbox string) []string
		wantErr bool
	}{
		{"one document per message", func(_, in string) []string {
			if in == "mb-other" {
				return nil
			}
			return []string{"E1"}
		}, false},
		{"a document per copy", func(_, in string) []string {
			if in == "mb-other" {
				return nil
			}
			if in == "" {
				return []string{"E1", "E2"}
			}
			return []string{"E1"}
		}, true},
		{"lookup ignores the folder terms", func(_, _ string) []string {
			return []string{"E1"}
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			copyQueryStub(t, copyFolderIDs, tc.ids)
			err := assertOneEmailIDForCopies("u1@example.com", "dfts1hit",
				copyFolderName, otherFolderName, 2)
			if tc.wantErr && err == nil {
				t.Error("the judgement accepted an arrangement it must refuse")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("assertOneEmailIDForCopies: %v", err)
			}
		})
	}
}

// A server answering the same hit in every folder must fail the folder the
// message was never copied into, and pass the one that holds it.
func TestFolderScopeAssertion(t *testing.T) {
	setFlag(t, flagTimeout, 100*time.Millisecond)
	c, _ := newFakeClient(t, 1)
	if err := assertHits(c, copyFolderName, "dfts1hit", 1); err != nil {
		t.Errorf("a folder holding the copy: %v", err)
	}
	if err := assertHits(c, otherFolderName, "dfts1hit", 0); err == nil {
		t.Error("a hit in a folder the message was never copied into was accepted")
	}
}

// -fts-copy-users carries one account per storage type; without it the check
// still runs for the single -fts-user.
func TestFTSCopyAccounts(t *testing.T) {
	setFlag(t, flagFTSUser, "u1@d.test")
	setFlag(t, flagFTSPass, "pw1")
	setFlag(t, flagFTSCopyUsers, "")
	if got := ftsCopyAccounts(); len(got) != 1 || got[0] != (ftsAccount{"u1@d.test", "pw1"}) {
		t.Errorf("fallback = %v, want the single -fts-user", got)
	}
	setFlag(t, flagFTSCopyUsers, "u1@d.test:pw1, u51@d.test:pw2 ,u101@d.test:pw3")
	got := ftsCopyAccounts()
	want := []ftsAccount{{"u1@d.test", "pw1"}, {"u51@d.test", "pw2"}, {"u101@d.test", "pw3"}}
	if len(got) != len(want) {
		t.Fatalf("parsed %d accounts, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("account %d = %v, want %v", i, got[i], want[i])
		}
	}
}
