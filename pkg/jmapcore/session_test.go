package jmapcore

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func testLimits() Limits {
	return Limits{
		BaseURL:               "https://mail.example.com",
		MaxSizeUpload:         41943040,
		MaxSizeRequest:        10485760,
		MaxConcurrentRequests: 10,
		MaxCallsInRequest:     16,
		MaxObjectsInGet:       500,
		MaxObjectsInSet:       500,
	}
}

// RFC 8620 §2 fixes the member names, so the wire form is asserted rather than
// the Go struct: a renamed json tag breaks every client silently.
func TestSessionWireShape(t *testing.T) {
	raw, err := json.Marshal(BuildSession(testLimits(), "u1@example.com"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{
		"capabilities", "accounts", "primaryAccounts", "username",
		"apiUrl", "downloadUrl", "uploadUrl", "eventSourceUrl", "state",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("session is missing %q", k)
		}
	}
	caps, _ := got["capabilities"].(map[string]any)
	for _, urn := range []string{CapCore, CapMail} {
		if _, ok := caps[urn]; !ok {
			t.Errorf("capabilities is missing %q", urn)
		}
	}
	if got["username"] != "u1@example.com" {
		t.Errorf("username = %v", got["username"])
	}
}

// The URLs are what a client follows next, so they must carry the configured
// public origin and the placeholders the RFC defines.
func TestSessionURLsUseBaseURL(t *testing.T) {
	sess := BuildSession(testLimits(), "u1@example.com")
	tests := []struct {
		name, got, wantContains string
	}{
		{"apiUrl", sess.APIURL, "https://mail.example.com/"},
		{"downloadUrl", sess.DownloadURL, "{accountId}"},
		{"downloadUrl blob", sess.DownloadURL, "{blobId}"},
		{"uploadUrl", sess.UploadURL, "{accountId}"},
		{"eventSourceUrl", sess.EventSourceURL, "{types}"},
	}
	for _, tt := range tests {
		if !strings.Contains(tt.got, tt.wantContains) {
			t.Errorf("%s = %q, want it to contain %q", tt.name, tt.got, tt.wantContains)
		}
	}
}

// A trailing slash on the configured origin must not produce doubled slashes.
func TestSessionBaseURLTrailingSlash(t *testing.T) {
	lim := testLimits()
	lim.BaseURL = "https://mail.example.com/"
	sess := BuildSession(lim, "u1@example.com")
	if strings.Contains(sess.APIURL, "//jmap") {
		t.Errorf("apiUrl has a doubled slash: %q", sess.APIURL)
	}
}

// The advertised limits come from the caller, since a client batches against
// them.
func TestSessionCoreLimitsFromCaller(t *testing.T) {
	sess := BuildSession(testLimits(), "u1@example.com")
	core, ok := sess.Capabilities[CapCore].(CoreCapability)
	if !ok {
		t.Fatalf("core capability has type %T", sess.Capabilities[CapCore])
	}
	if core.MaxSizeUpload != 41943040 {
		t.Errorf("maxSizeUpload = %d", core.MaxSizeUpload)
	}
	if core.MaxObjectsInGet != 500 || core.MaxCallsInRequest != 16 {
		t.Errorf("limits not carried: %+v", core)
	}
	if len(core.CollationAlgorithms) == 0 {
		t.Error("collationAlgorithms must not be empty")
	}
}

// The account is the user's own and mail is its primary account.
func TestSessionAccount(t *testing.T) {
	const user = "u1@example.com"
	sess := BuildSession(testLimits(), user)
	acct, ok := sess.Accounts[AccountID(user)]
	if !ok {
		t.Fatalf("no account for %q: %v", user, sess.Accounts)
	}
	if !acct.IsPersonal || acct.IsReadOnly {
		t.Errorf("account flags: %+v", acct)
	}
	if _, ok := acct.AccountCapabilities[CapMail]; !ok {
		t.Error("account is missing the mail capability")
	}
	if sess.PrimaryAccounts[CapMail] != AccountID(user) {
		t.Errorf("primaryAccounts[mail] = %q", sess.PrimaryAccounts[CapMail])
	}
}

// Unlimited mail limits must serialise as null, not 0: a client reads 0 as
// "none allowed".
func TestMailCapabilityUnlimitedIsNull(t *testing.T) {
	raw, err := json.Marshal(MailCapabilityFor())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"maxMailboxesPerEmail", "maxMailboxDepth"} {
		if v, ok := got[k]; !ok || v != nil {
			t.Errorf("%s = %v, want null", k, v)
		}
	}
}

// The state changes when an advertised limit does, and only then: a client
// caches the session against it.
func TestSessionStateTracksLimits(t *testing.T) {
	base := SessionState(testLimits())
	if base != SessionState(testLimits()) {
		t.Error("state is not stable for identical limits")
	}
	changed := testLimits()
	changed.MaxObjectsInGet = 100
	if SessionState(changed) == base {
		t.Error("state did not change with an advertised limit")
	}
	rebased := testLimits()
	rebased.BaseURL = "https://other.example.com"
	if SessionState(rebased) == base {
		t.Error("state did not change with the base URL")
	}
}

// The session state moves with the advertised Sieve capability.
func TestSessionStateCarriesSieve(t *testing.T) {
	lim := testLimits()
	plain := SessionState(lim)
	lim.Sieve = &SieveSession{Implementation: "x", Account: SieveCapability{MaxSizeScriptName: 512}}
	with := SessionState(lim)
	lim.Sieve.Account.MaxSizeScriptName = 1024
	changed := SessionState(lim)
	if plain == with || with == changed {
		t.Errorf("states %s, %s, %s: want each to differ", plain, with, changed)
	}
}

// Submission is advertised at the session and the account, with its own
// primary account, and moves the session state.
func TestSessionCarriesSubmission(t *testing.T) {
	lim := testLimits()
	plain := SessionState(lim)
	lim.Submission = &SubmissionCapability{SubmissionExtensions: map[string][]string{"SIZE": {"100"}}}
	sess := BuildSession(lim, "u1@example.com")
	if _, ok := sess.Capabilities[CapSubmission]; !ok {
		t.Error("no session-level submission capability")
	}
	if got := sess.Accounts[AccountID("u1@example.com")].AccountCapabilities[CapSubmission]; got != lim.Submission {
		t.Errorf("account capability = %v", got)
	}
	if sess.PrimaryAccounts[CapSubmission] != AccountID("u1@example.com") {
		t.Errorf("primary account = %q", sess.PrimaryAccounts[CapSubmission])
	}
	if sess.State == plain {
		t.Error("the session state did not move with the submission capability")
	}
}

func TestSessionCarriesVacation(t *testing.T) {
	lim := testLimits()
	plain := SessionState(lim)
	if _, ok := BuildSession(lim, "u1@example.com").Capabilities[CapVacation]; ok {
		t.Error("vacation advertised without it")
	}
	lim.Vacation = &VacationCapability{}
	sess := BuildSession(lim, "u1@example.com")
	if _, ok := sess.Capabilities[CapVacation]; !ok {
		t.Error("no session-level vacation capability")
	}
	if _, ok := sess.Accounts[AccountID("u1@example.com")].AccountCapabilities[CapVacation]; !ok || sess.PrimaryAccounts[CapVacation] != AccountID("u1@example.com") {
		t.Errorf("account capability or primary account missing: %+v", sess)
	}
	if sess.State == plain {
		t.Error("the session state did not move with the vacation capability")
	}
}

// Every id the session names is an RFC 8620 §1.2 Id: 1-255 characters of the
// URL-safe base64 alphabet. A client library that checks it refuses the
// whole session otherwise.
func TestSessionIDsAreJMAPIds(t *testing.T) {
	for _, user := range []string{"u1@example.com", "first.last+tag@sub.example.org", "u", "юзер@приклад.укр"} {
		sess := BuildSession(testLimits(), user)
		ids := []string{}
		for id := range sess.Accounts {
			ids = append(ids, id)
		}
		for _, id := range sess.PrimaryAccounts {
			ids = append(ids, id)
		}
		for _, id := range ids {
			if !validID(id) {
				t.Errorf("%s: account id %q is not a JMAP Id", user, id)
			}
		}
		if AccountID(user) == AccountID(user+"x") {
			t.Errorf("%s: two users share an account id", user)
		}
	}
}

// validID is RFC 8620 §1.2 with the leading character a client library also
// insists on: alphanumeric first, then the URL-safe alphabet.
func validID(id string) bool {
	return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,254}$`).MatchString(id)
}
