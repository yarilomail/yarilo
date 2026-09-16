package jmap

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// quotaServer delivers one message and serves it under the given rules, so the
// numbers a row reads are a real count of a real mailbox.
func quotaServer(t *testing.T, rules []string, policy quota.Policy, enabled bool) (*Server, int64) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/", QuotaRules: rules}
	locker := &testLocker{}

	mb := maildir.New()
	box := mb.OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	idx := file.New(file.WithLocker(locker))
	ui := idx.OpenUser(info)

	raw := "Subject: quota probe\r\n\r\n" + strings.Repeat("x", 512)
	name, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), 1, int64(len(raw)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	meta := &mailbox.MessageMeta{UID: 1, Size: uint32(len(raw)), VSize: vsize, GUID: guid, InternalDate: time.Now()}
	if err := mailboxbase.NameSaved(box, "INBOX", name, meta); err != nil {
		t.Fatalf("name: %v", err)
	}
	if err := ui.AppendMessage(f.ID, meta); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := ui.Close(); err != nil {
		t.Fatalf("index close: %v", err)
	}

	s := New(Options{
		Trust:        ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits:       testLimits(),
		QuotaPolicy:  policy,
		QuotaEnabled: enabled,
		Storage: &Storage{
			Mailbox:     maildir.New(),
			Index:       file.New(file.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      locker,
		},
	})
	return s, int64(vsize)
}

func quotaCall(t *testing.T, s *Server, method, args string) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:quota"],"methodCalls":[["%s",%s,"c0"]]}`, method, args)
	w := postAPIRaw(t, s, body)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d: %s", method, w.Code, w.Body.String())
	}
	var resp struct {
		MethodResponses []json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.MethodResponses) != 1 {
		t.Fatalf("%s: %d responses, want 1", method, len(resp.MethodResponses))
	}
	var triple []json.RawMessage
	if err := json.Unmarshal(resp.MethodResponses[0], &triple); err != nil {
		t.Fatalf("decode triple: %v", err)
	}
	var name string
	if err := json.Unmarshal(triple[0], &name); err != nil {
		t.Fatalf("decode name: %v", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(triple[1], &out); err != nil {
		t.Fatalf("decode args: %v", err)
	}
	out["__name"] = name
	return out
}

// Quota/get answers the object RFC 9425 §1.3 describes, filled from the count
// IMAP reads: one root, octets, with the account's own limit.
func TestQuotaGetAnswersTheAccountRoot(t *testing.T) {
	s, vsize := quotaServer(t, []string{"*:storage=10M"}, quota.Policy{StoragePercentage: 100, MessagePercentage: 100}, true)

	got := quotaCall(t, s, "Quota/get", `{"accountId":"`+testUser+`"}`)
	if got["__name"] != "Quota/get" {
		t.Fatalf("answered %v: %v", got["__name"], got)
	}
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list has %d objects, want the one octets root: %v", len(list), got)
	}
	q, _ := list[0].(map[string]any)
	for field, want := range map[string]any{
		"id": "octets", "resourceType": "octets", "scope": "account", "name": "User quota",
	} {
		if q[field] != want {
			t.Errorf("%s = %v, want %v", field, q[field], want)
		}
	}
	if used, _ := q["used"].(float64); int64(used) != vsize {
		t.Errorf("used = %v, want the counted %d", q["used"], vsize)
	}
	if hard, _ := q["hardLimit"].(float64); int64(hard) != 10*1024*1024 {
		t.Errorf("hardLimit = %v, want 10 MiB in octets", q["hardLimit"])
	}
	types, _ := q["types"].([]any)
	if len(types) != 1 || types[0] != "Mail" {
		t.Errorf("types = %v, want [Mail]", q["types"])
	}
	if _, present := q["warnLimit"]; !present {
		t.Error("warnLimit is absent; the RFC has it nullable, not optional")
	}
}

// A message limit is a second root, and only when one is configured: a count
// root with no limit would report a bound nobody set.
func TestTheCountRootAppearsOnlyWithAMessageLimit(t *testing.T) {
	rows := []struct {
		name  string
		rules []string
		want  []string
	}{
		{"storage only", []string{"*:storage=10M"}, []string{"octets"}},
		{"storage and messages", []string{"*:storage=10M:messages=100"}, []string{"count", "octets"}},
		{"neither", nil, nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s, _ := quotaServer(t, row.rules, quota.Policy{StoragePercentage: 100, MessagePercentage: 100}, true)
			got := quotaCall(t, s, "Quota/query", `{"accountId":"`+testUser+`"}`)
			ids, _ := got["ids"].([]any)
			var names []string
			for _, id := range ids {
				names = append(names, fmt.Sprint(id))
			}
			if strings.Join(names, ",") != strings.Join(row.want, ",") {
				t.Errorf("ids = %v, want %v", names, row.want)
			}
		})
	}
}

// The engine off means the limits in userdb are not enforced; reporting them
// would announce a bound nothing applies.
func TestNoQuotaObjectsWhenTheEngineIsOff(t *testing.T) {
	s, _ := quotaServer(t, []string{"*:storage=10M"}, quota.Policy{StoragePercentage: 100, MessagePercentage: 100}, false)
	got := quotaCall(t, s, "Quota/get", `{"accountId":"`+testUser+`"}`)
	if list, _ := got["list"].([]any); len(list) != 0 {
		t.Errorf("list = %v, want nothing while the engine is off", got["list"])
	}
}

// Quota/changes names the object whose numbers moved, and nothing when they
// did not: a client polling a still mailbox must see an empty answer.
func TestQuotaChangesNamesWhatMoved(t *testing.T) {
	s, _ := quotaServer(t, []string{"*:storage=10M"}, quota.Policy{StoragePercentage: 100, MessagePercentage: 100}, true)
	now := quotaCall(t, s, "Quota/get", `{"accountId":"`+testUser+`"}`)
	state, _ := now["state"].(string)
	if state == "" {
		t.Fatal("Quota/get returned no state")
	}

	same := quotaCall(t, s, "Quota/changes", `{"accountId":"`+testUser+`","sinceState":"`+state+`"}`)
	if updated, _ := same["updated"].([]any); len(updated) != 0 {
		t.Errorf("updated = %v against its own state, want nothing", same["updated"])
	}
	if same["newState"] != state {
		t.Errorf("newState = %v, want the unchanged %q", same["newState"], state)
	}

	// A state from a lighter mailbox: the octets root moved since.
	moved := quotaCall(t, s, "Quota/changes", `{"accountId":"`+testUser+`","sinceState":"q-0-0-10485760-0"}`)
	updated, _ := moved["updated"].([]any)
	if len(updated) != 1 || updated[0] != "octets" {
		t.Errorf("updated = %v, want [octets]", moved["updated"])
	}
}

// A state this server never issued cannot be compared against: saying so is
// what sends a client back to Quota/get instead of trusting a guess.
func TestQuotaChangesRefusesAForeignState(t *testing.T) {
	s, _ := quotaServer(t, []string{"*:storage=10M"}, quota.Policy{StoragePercentage: 100, MessagePercentage: 100}, true)
	got := quotaCall(t, s, "Quota/changes", `{"accountId":"`+testUser+`","sinceState":"not-a-state"}`)
	if got["__name"] != "error" {
		t.Fatalf("answered %v, want an error: %v", got["__name"], got)
	}
	if got["type"] != "cannotCalculateChanges" {
		t.Errorf("type = %v, want cannotCalculateChanges", got["type"])
	}
}

// Quota/queryChanges answers rather than refusing: the result set is the
// account's roots, and the state names which of them existed.
func TestQuotaQueryChangesReportsTheRootThatAppeared(t *testing.T) {
	s, _ := quotaServer(t, []string{"*:storage=10M:messages=100"}, quota.Policy{StoragePercentage: 100, MessagePercentage: 100}, true)
	// A state from when only storage was limited.
	got := quotaCall(t, s, "Quota/queryChanges", `{"accountId":"`+testUser+`","sinceQueryState":"q-0-0-10485760-0"}`)
	added, _ := got["added"].([]any)
	if len(added) != 1 {
		t.Fatalf("added = %v, want the count root alone", got["added"])
	}
	item, _ := added[0].(map[string]any)
	if item["id"] != "count" {
		t.Errorf("added id = %v, want count", item["id"])
	}
	if removed, _ := got["removed"].([]any); len(removed) != 0 {
		t.Errorf("removed = %v, want nothing", got["removed"])
	}
}

// The percentage scaling applies to what JMAP reports, or a client reads one
// limit while delivery enforces another.
func TestTheReportedLimitIsTheScaledOne(t *testing.T) {
	s, _ := quotaServer(t, []string{"*:storage=10M"}, quota.Policy{StoragePercentage: 50, MessagePercentage: 100}, true)
	got := quotaCall(t, s, "Quota/get", `{"accountId":"`+testUser+`"}`)
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v", got["list"])
	}
	q, _ := list[0].(map[string]any)
	if hard, _ := q["hardLimit"].(float64); int64(hard) != 5*1024*1024 {
		t.Errorf("hardLimit = %v, want the scaled 5 MiB", q["hardLimit"])
	}
}
