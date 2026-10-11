package quotastatus_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/quotastatus"
	file "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/memory"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

func newMemDict(t *testing.T) dict.Dict {
	t.Helper()
	d, err := memory.New(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("memory dict: %v", err)
	}
	return d
}

func startServer(t *testing.T, opts quotastatus.Options) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := quotastatus.New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, ln) }() //nolint:errcheck
	return ln.Addr().String()
}

func policyCheck(t *testing.T, addr string, attrs map[string]string) string {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	var sb strings.Builder
	for k, v := range attrs {
		fmt.Fprintf(&sb, "%s=%s\n", k, v)
	}
	sb.WriteString("\n")
	if _, err := fmt.Fprint(conn, sb.String()); err != nil {
		t.Fatalf("write request: %v", err)
	}

	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "action=") {
			return strings.TrimPrefix(line, "action=")
		}
	}
	t.Fatal("no action in response")
	return ""
}

// startStorageServer builds a quota-status server backed by real maildir+index
// storage: each user in userBytes gets one INBOX message of that virtual size,
// and UserdbLookup returns quotaRules. This mirrors production, where the count
// backend sums the recipient's index.
func startStorageServer(t *testing.T, quotaRules []string, aliasD dict.Dict, hops int, userBytes map[string]uint32) string {
	return startStorageServerOpts(t, quotaRules, aliasD, hops, userBytes, nil)
}

// startStorageServerOpts is startStorageServer with a hook to tweak the final
// Options (recipient_delimiter, mail_size, exceeded_message, ...).
func startStorageServerOpts(t *testing.T, quotaRules []string, aliasD dict.Dict, hops int, userBytes map[string]uint32, mutate func(*quotastatus.Options)) string {
	t.Helper()
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	mb := maildir.New()
	idx := file.New()
	for user, b := range userBytes {
		ui, _ := resolver.UserInfo(user, "")
		box := mb.OpenUser(ui)
		_ = box.Init()
		box.Close() //nolint:errcheck
		uidx := idx.OpenUser(ui)
		f, err := uidx.OpenFolder("INBOX", 1)
		if err != nil {
			t.Fatalf("open INBOX for %s: %v", user, err)
		}
		if b > 0 {
			if err := uidx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, VSize: b, Size: b}); err != nil {
				t.Fatalf("append for %s: %v", user, err)
			}
		}
		uidx.Close() //nolint:errcheck
	}
	lookup := func(_ context.Context, username string) (*mailbox.UserInfo, error) {
		mi, _ := resolver.UserInfo(username, "")
		mi.QuotaRules = quotaRules
		return mi, nil
	}
	opts := quotastatus.Options{
		Enabled:      true,
		Limits:       quota.ParseRules(quotaRules),
		UserdbLookup: lookup,
		Mailbox:      mb,
		Index:        idx,
		AliasDict:    aliasD,
		AliasMaxHops: hops,
		Policy:       quota.Policy{StoragePercentage: 100, MessagePercentage: 100},
	}
	if mutate != nil {
		mutate(&opts)
	}
	return startServer(t, opts)
}

func TestPolicyCheck_RecipientDelimiter(t *testing.T) {
	// With delimiter "-", the "-Spam" detail selects the ignored folder so an
	// otherwise-over-quota user is allowed; "+" is treated as a literal.
	addr := startStorageServerOpts(t, []string{"*:storage=1K", "Spam:ignore"}, nil, 0,
		map[string]uint32{"alice@example.com": 2048},
		func(o *quotastatus.Options) { o.RecipientDelimiter = "-" })
	if a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice-Spam@example.com", "size": "100",
	}); a != "OK" {
		t.Errorf("want OK for delimiter-ignored folder, got %q", a)
	}
	// "+" is no longer a delimiter, so alice+Spam maps to a different username.
	if a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice+Spam@example.com", "size": "100",
	}); !strings.HasPrefix(a, "OK") && !strings.HasPrefix(a, "554") && !strings.HasPrefix(a, "DEFER_IF_PERMIT") {
		t.Errorf("unexpected action %q", a)
	}
}

func TestPolicyCheck_MailSize(t *testing.T) {
	addr := startStorageServerOpts(t, []string{"*:storage=10M"}, nil, 0,
		map[string]uint32{"alice@example.com": 1},
		func(o *quotastatus.Options) { o.MailSize = 500 })
	// Under the cap → allowed.
	if a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "100",
	}); a != "OK" {
		t.Errorf("want OK under mail_size, got %q", a)
	}
	// Over the cap → distinct 552 rejection.
	a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "600",
	})
	if !strings.HasPrefix(a, "554 5.2.2") || !strings.Contains(a, "maximum size allowed") {
		t.Errorf("want 554 5.2.2 with the max-size reason, got %q", a)
	}
}

func TestPolicyCheck_ExceededMessage(t *testing.T) {
	addr := startStorageServerOpts(t, []string{"*:storage=1K"}, nil, 0,
		map[string]uint32{"alice@example.com": 1024},
		func(o *quotastatus.Options) { o.ExceededMessage = "over the top" })
	a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "100",
	})
	if !strings.HasPrefix(a, "554 5.2.2") || !strings.Contains(a, "over the top") {
		t.Errorf("want custom exceeded message, got %q", a)
	}
}

func TestPolicyCheck_UnderQuota(t *testing.T) {
	addr := startStorageServer(t, []string{"*:storage=10M"}, nil, 0, map[string]uint32{"alice@example.com": 1})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "1024",
	})
	if action != "OK" {
		t.Errorf("want OK, got %q", action)
	}
}

func TestPolicyCheck_OverQuota(t *testing.T) {
	addr := startStorageServer(t, []string{"*:storage=1K"}, nil, 0, map[string]uint32{"alice@example.com": 1024})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "100",
	})
	if !strings.HasPrefix(action, "554 5.2.2") {
		t.Errorf("want 554 5.2.2, got %q", action)
	}
}

func TestPolicyCheck_IgnoreFolder(t *testing.T) {
	addr := startStorageServer(t, []string{"*:storage=1K", "Spam:ignore"}, nil, 0, map[string]uint32{"alice@example.com": 2048})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice+Spam@example.com", "size": "100",
	})
	if action != "OK" {
		t.Errorf("want OK for ignored folder, got %q", action)
	}
}

func TestPolicyCheck_NoStorage(t *testing.T) {
	// No Mailbox/Index/UserdbLookup wired → fail-open.
	addr := startServer(t, quotastatus.Options{Limits: quota.ParseRules([]string{"*:storage=1K"}),
		Policy: quota.Policy{StoragePercentage: 100, MessagePercentage: 100}})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "9999",
	})
	if action != "OK" {
		t.Errorf("want OK when no storage, got %q", action)
	}
}

func setAlias(t *testing.T, d dict.Dict, src, dst string) {
	t.Helper()
	tx, err := d.Begin(context.Background(), &dict.OpSettings{})
	if err != nil {
		t.Fatalf("alias begin: %v", err)
	}
	if err := tx.Set(src, []byte(dst)); err != nil {
		t.Fatalf("alias set: %v", err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("alias commit: %v", err)
	}
}

func TestAliasResolution_DirectAlias(t *testing.T) {
	aliasD := newMemDict(t)
	setAlias(t, aliasD, "info@example.com", "alice@example.com")
	addr := startStorageServer(t, []string{"*:storage=1K"}, aliasD, 5, map[string]uint32{"alice@example.com": 2048})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "info@example.com", "size": "100",
	})
	if !strings.HasPrefix(action, "554 5.2.2") {
		t.Errorf("alias resolved to over-quota alice: want 554 5.2.2, got %q", action)
	}
}

// alias_max_hops 0 follows no alias: the recipient is checked as written.
func TestAliasResolution_ZeroHopsFollowsNothing(t *testing.T) {
	aliasD := newMemDict(t)
	setAlias(t, aliasD, "info@example.com", "alice@example.com")
	addr := startStorageServer(t, []string{"*:storage=1K"}, aliasD, 0, map[string]uint32{"alice@example.com": 2048})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "info@example.com", "size": "100",
	})
	if strings.HasPrefix(action, "554 5.2.2") {
		t.Errorf("0 hops followed the alias to over-quota alice: %q", action)
	}
}

func TestAliasResolution_ChainedAlias(t *testing.T) {
	aliasD := newMemDict(t)
	setAlias(t, aliasD, "sales@example.com", "info@example.com")
	setAlias(t, aliasD, "info@example.com", "alice@example.com")
	addr := startStorageServer(t, []string{"*:storage=1K"}, aliasD, 5, map[string]uint32{"alice@example.com": 2048})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "sales@example.com", "size": "100",
	})
	if !strings.HasPrefix(action, "554 5.2.2") {
		t.Errorf("chained alias: want 554 5.2.2, got %q", action)
	}
}

func TestAliasResolution_DetailStrippedForLookup(t *testing.T) {
	aliasD := newMemDict(t)
	setAlias(t, aliasD, "info@example.com", "alice@example.com")
	addr := startStorageServer(t, []string{"*:storage=1K"}, aliasD, 5, map[string]uint32{"alice@example.com": 2048})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "info+newsletter@example.com", "size": "100",
	})
	if !strings.HasPrefix(action, "554 5.2.2") {
		t.Errorf("detail+alias: want 554 5.2.2, got %q", action)
	}
}

func TestAliasResolution_NoAlias_FallsBackToDirect(t *testing.T) {
	aliasD := newMemDict(t)
	addr := startStorageServer(t, []string{"*:storage=10M"}, aliasD, 5, map[string]uint32{"bob@example.com": 0})
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "bob@example.com", "size": "100",
	})
	if action != "OK" {
		t.Errorf("no alias, under quota: want OK, got %q", action)
	}
}

func TestPolicyCheck_MultipleRequestsPerConn(t *testing.T) {
	addr := startStorageServer(t, []string{"*:storage=10M"}, nil, 0, map[string]uint32{"bob@example.com": 0})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for i := 0; i < 3; i++ {
		fmt.Fprintf(conn, "request=smtpd_access_policy\nrecipient=bob@example.com\nsize=100\n\n") //nolint:errcheck
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "action=") {
				if line != "action=OK" {
					t.Errorf("request %d: want action=OK, got %q", i, line)
				}
				break
			}
		}
	}
}

func TestPolicyCheck_Nouser(t *testing.T) {
	mb := maildir.New()
	idx := file.New()
	lookup := func(_ context.Context, _ string) (*mailbox.UserInfo, error) {
		return nil, nil // recipient unknown in userdb
	}
	base := quotastatus.Options{
		Enabled:      true,
		Limits:       quota.ParseRules([]string{"*:storage=1K"}),
		UserdbLookup: lookup,
		Mailbox:      mb,
		Index:        idx,
		Policy:       quota.Policy{StoragePercentage: 100, MessagePercentage: 100},
	}
	req := map[string]string{"request": "smtpd_access_policy", "recipient": "ghost@example.com", "size": "100"}

	// Configured nouser action is returned for an unknown recipient.
	b := base
	b.Nouser = "REJECT Unknown user"
	if a := policyCheck(t, startServer(t, b), req); !strings.HasPrefix(a, "REJECT") || !strings.Contains(a, "Unknown user") {
		t.Errorf("want REJECT Unknown user, got %q", a)
	}
	// Empty nouser opts out to DUNNO.
	if a := policyCheck(t, startServer(t, base), req); a != "DUNNO" {
		t.Errorf("empty nouser should DUNNO, got %q", a)
	}
}

// An MTA that sends no size at RCPT still has a full mailbox refused, grace or
// not: the check allocates at least one byte, never zero.
func TestPolicyCheck_OverQuotaWithoutSizeAndWithGrace(t *testing.T) {
	addr := startStorageServerOpts(t, []string{"*:storage=1K"}, nil, 0, map[string]uint32{"alice@example.com": 1024},
		func(o *quotastatus.Options) { o.Policy.StorageGrace = 10 << 20 })
	action := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com",
	})
	if !strings.HasPrefix(action, "554 5.2.2") {
		t.Errorf("a full mailbox with no size given answered %q, want 554 5.2.2", action)
	}
}

// Every answer is the configured action, with the reason in %{error}; an empty
// toolarge action falls back to the over-quota one.
func TestPolicyCheck_ActionsAreConfigured(t *testing.T) {
	custom := func(o *quotastatus.Options) {
		o.Success, o.Toolarge, o.Overquota = "DUNNO", "REJECT 552 too big: %{error}", "REJECT 452 4.2.2 full: %{error}"
		o.MailSize = 5000
	}
	fallback := func(o *quotastatus.Options) { o.Overquota, o.MailSize = "DEFER_IF_PERMIT %{error}", 5000 }
	for _, tc := range []struct {
		name   string
		used   uint32
		size   string
		mutate func(*quotastatus.Options)
		want   string
	}{
		{"success", 100, "100", custom, "DUNNO"},
		{"over quota", 1024, "100", custom, "REJECT 452 4.2.2 full: Mailbox full"},
		{"over the mail size", 100, "6000", custom, "REJECT 552 too big: Mail size is larger than the maximum size allowed by server configuration"},
		{"larger than the whole limit", 100, "2000", custom, "REJECT 552 too big: Mailbox full"},
		{"empty toolarge uses overquota", 100, "6000", fallback, "DEFER_IF_PERMIT Mail size is larger than the maximum size allowed by server configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := startStorageServerOpts(t, []string{"*:storage=1K"}, nil, 0, map[string]uint32{"alice@example.com": tc.used}, tc.mutate)
			if a := policyCheck(t, addr, map[string]string{
				"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": tc.size,
			}); a != tc.want {
				t.Errorf("answered %q, want %q", a, tc.want)
			}
		})
	}
}

// A userdb that cannot answer defers the recipient; it neither accepts the
// mail unchecked nor refuses it for good.
func TestPolicyCheck_UserdbErrorDefers(t *testing.T) {
	addr := startServer(t, quotastatus.Options{
		Enabled: true, Limits: quota.ParseRules([]string{"*:storage=1K"}),
		UserdbLookup: func(context.Context, string) (*mailbox.UserInfo, error) { return nil, errors.New("down") },
		Mailbox:      maildir.New(), Index: file.New(),
		Policy: quota.Policy{StoragePercentage: 100, MessagePercentage: 100},
	})
	if a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "100",
	}); !strings.HasPrefix(a, "DEFER_IF_PERMIT") {
		t.Errorf("a userdb failure answered %q, want DEFER_IF_PERMIT", a)
	}
}

// INBOX at its message cap is full whatever the storage says; one below it
// takes the message, as LMTP would.
func TestPolicyCheck_MailboxMessageCount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules []string
		cap   int64
		want  string
	}{
		{"at the cap", []string{"*:storage=1M"}, 1, "554 5.2.2 Too many messages in the mailbox"},
		{"below the cap", []string{"*:storage=1M"}, 2, "OK"},
		{"at the cap with no storage limit", nil, 1, "554 5.2.2 Too many messages in the mailbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := startStorageServerOpts(t, tc.rules, nil, 0, map[string]uint32{"alice@example.com": 100},
				func(o *quotastatus.Options) { o.Policy.MailboxMessageCount = tc.cap })
			if a := policyCheck(t, addr, map[string]string{
				"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "100",
			}); a != tc.want {
				t.Errorf("answered %q, want %q", a, tc.want)
			}
		})
	}
}

// A message larger than the limit that still fits within grace is accepted:
// it is too large only once it does not fit at all.
func TestPolicyCheck_LargerThanTheLimitWithinGraceFits(t *testing.T) {
	addr := startStorageServerOpts(t, []string{"*:storage=1K"}, nil, 0, map[string]uint32{"alice@example.com": 0},
		func(o *quotastatus.Options) { o.Policy.StorageGrace = 10 << 20 })
	if a := policyCheck(t, addr, map[string]string{
		"request": "smtpd_access_policy", "recipient": "alice@example.com", "size": "2000",
	}); a != "OK" {
		t.Errorf("a 2000-byte message into an empty mailbox with a 1K limit and 10M grace answered %q, want OK", a)
	}
}
