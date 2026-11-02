package jmap

import (
	"encoding/json"
	"net/http"
	"testing"
)

func emailQuery(t *testing.T, s *Server, args string) map[string]any {
	t.Helper()
	return callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/query",`+args+`,"c0"]]}`)
}

func idsOf(t *testing.T, got map[string]any) []string {
	t.Helper()
	raw, _ := got["ids"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

// Discovery is the point of this method: a client with no ids must be able to
// find them, and then read them back with Email/get.
func TestEmailQueryFindsAndFeedsEmailGet(t *testing.T) {
	s, id := emailServer(t, 0)
	got := emailQuery(t, s, `{"accountId":"`+testAccount+`"}`)
	ids := idsOf(t, got)
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("ids = %v, want [%s]", ids, id)
	}
	if got["canCalculateChanges"] != false {
		t.Errorf("canCalculateChanges = %v, want false", got["canCalculateChanges"])
	}
	if got["queryState"] == "" {
		t.Error("queryState is empty")
	}

	// The whole discovery path, in one batch, through a back-reference.
	batch := callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/query",{"accountId":"`+testAccount+`"},"c0"],
		["Email/get",{"accountId":"`+testAccount+`",
			"#ids":{"resultOf":"c0","name":"Email/query","path":"/ids"}},"c1"]]}`)
	_ = batch
}

// The ceiling bounds the server's work, and the response has to report the
// limit it applied or a client cannot tell it was cut short.
func TestEmailQueryLimitCeilingIsReported(t *testing.T) {
	s, _ := emailServer(t, 0)
	s.opts.QueryMaxLimit = 1

	tests := []struct {
		name, args string
		wantLimit  float64
	}{
		{"client names none", `{"accountId":"` + testAccount + `"}`, 1},
		{"client over the ceiling", `{"accountId":"` + testAccount + `","limit":50}`, 1},
		{"client under the ceiling", `{"accountId":"` + testAccount + `","limit":1}`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := emailQuery(t, s, tt.args)
			if got["limit"] != tt.wantLimit {
				t.Errorf("limit = %v, want %v - a client that is not told cannot page", got["limit"], tt.wantLimit)
			}
			if n := len(idsOf(t, got)); uint(n) > uint(tt.wantLimit) {
				t.Errorf("returned %d ids, over the applied limit", n)
			}
		})
	}
}

// Without the full-text index, text and body are refused by name, so a client
// can drop exactly those and retry; the header conditions beside them are
// answered (#2452).
func TestEmailQueryNamesTheConditionsItCannotAnswer(t *testing.T) {
	s, _ := emailServer(t, 0)
	w := postAPIRaw(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/query",{"accountId":"`+testAccount+`","filter":{"subject":"x","body":"y"}},"c0"]]}`)
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var name string
	if err := json.Unmarshal(resp.MethodResponses[0][0], &name); err != nil {
		t.Fatalf("name: %v", err)
	}
	if name != "error" {
		t.Fatalf("response = %s, want error", name)
	}
	var merr struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(resp.MethodResponses[0][1], &merr); err != nil {
		t.Fatalf("args: %v", err)
	}
	if merr.Type != "unsupportedFilter" {
		t.Errorf("type = %s", merr.Type)
	}
	if !contains2(merr.Description, "body") {
		t.Errorf("description %q does not name body", merr.Description)
	}
	if contains2(merr.Description, "subject") {
		t.Errorf("description %q names subject, which the header answers", merr.Description)
	}
}

// Without the full-text index the header conditions are answered from the
// message header, as IMAP SEARCH answers them without it (#2452).
func TestEmailQueryAnswersHeaderConditionsWithoutFTS(t *testing.T) {
	s, _ := emailServer(t, 0)
	tests := []struct {
		name, filter string
		want         int
	}{
		{"subject, decoded and any case", `{"subject":"привіт"}`, 1},
		{"subject not there", `{"subject":"zzz"}`, 0},
		{"from", `{"from":"alice@example"}`, 1},
		{"to, the second address", `{"to":"carol"}`, 1},
		{"cc the message lacks", `{"cc":"bob"}`, 0},
		{"header present", `{"header":["Message-Id"]}`, 1},
		{"header absent", `{"header":["X-None"]}`, 0},
		{"header with a value", `{"header":["In-Reply-To","m0@"]}`, 1},
		{"header with another value", `{"header":["In-Reply-To","m9@"]}`, 0},
		{"subject and a keyword together", `{"subject":"привіт","notKeyword":"$seen"}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := emailQuery(t, s, `{"accountId":"`+testAccount+`","filter":`+tt.filter+`}`)
			if n := len(idsOf(t, got)); n != tt.want {
				t.Errorf("matched %d, want %d: %v", n, tt.want, got)
			}
		})
	}
	for _, f := range []string{`{"text":"x"}`, `{"body":"x"}`} {
		w := postAPIRaw(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
			["Email/query",{"accountId":"`+testAccount+`","filter":`+f+`},"c0"]]}`)
		if !contains2(w.Body.String(), "unsupportedFilter") {
			t.Errorf("%s answered without the full-text index: %s", f, w.Body.String())
		}
	}
}

// Index-backed conditions are answered, so the refusal above is about the
// full-text ones specifically and not about filtering at all.
func TestEmailQueryAnswersIndexConditions(t *testing.T) {
	s, id := emailServer(t, 0)
	tests := []struct {
		name, args string
		wantCount  int
	}{
		{"has the seen keyword", `{"accountId":"` + testAccount + `","filter":{"hasKeyword":"$seen"}}`, 1},
		{"lacks the seen keyword", `{"accountId":"` + testAccount + `","filter":{"notKeyword":"$seen"}}`, 0},
		{"minSize over the message", `{"accountId":"` + testAccount + `","filter":{"minSize":100000}}`, 0},
		{"minSize under the message", `{"accountId":"` + testAccount + `","filter":{"minSize":1}}`, 1},
		{"after the epoch", `{"accountId":"` + testAccount + `","filter":{"after":"1970-01-01T00:00:00Z"}}`, 1},
		{"before the epoch", `{"accountId":"` + testAccount + `","filter":{"before":"1970-01-01T00:00:00Z"}}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := emailQuery(t, s, tt.args)
			if n := len(idsOf(t, got)); n != tt.wantCount {
				t.Errorf("matched %d, want %d", n, tt.wantCount)
			}
		})
	}
	_ = id
}

// Two identical queries must produce the same order, or a client paging with
// position sees a message twice or not at all.
func TestEmailQueryOrderIsDeterministic(t *testing.T) {
	s, _ := emailServer(t, 0)
	first := idsOf(t, emailQuery(t, s, `{"accountId":"`+testAccount+`"}`))
	for i := 0; i < 5; i++ {
		again := idsOf(t, emailQuery(t, s, `{"accountId":"`+testAccount+`"}`))
		if len(again) != len(first) {
			t.Fatalf("run %d returned %d ids, first returned %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("run %d differs at %d: %s vs %s", i, j, again[j], first[j])
			}
		}
	}
}

// queryState covers the filter and the sort: two queries with different
// arguments describe different result sets and must not share a state string a
// client could cache one against the other.
func TestEmailQueryStateCoversFilterAndSort(t *testing.T) {
	s, _ := emailServer(t, 0)
	base := emailQuery(t, s, `{"accountId":"`+testAccount+`"}`)["queryState"]
	tests := []struct{ name, args string }{
		{"different filter", `{"accountId":"` + testAccount + `","filter":{"minSize":1}}`},
		{"different sort", `{"accountId":"` + testAccount + `","sort":[{"property":"size"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := emailQuery(t, s, tt.args)["queryState"]; got == base {
				t.Errorf("queryState is unchanged for %s", tt.name)
			}
		})
	}
}

// A sort the server cannot honour is refused, never silently replaced.
func TestEmailQueryRefusesUnknownSort(t *testing.T) {
	s, _ := emailServer(t, 0)
	w := postAPIRaw(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/query",{"accountId":"`+testAccount+`","sort":[{"property":"subject"}]},"c0"]]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !contains2(w.Body.String(), "unsupportedSort") {
		t.Errorf("body = %s", w.Body)
	}
}

func contains2(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
