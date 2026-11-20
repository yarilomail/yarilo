package lmtp

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailops"
	"github.com/yarilomail/yarilo/v2/internal/userstate/threads"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The full delivery path at two folder sizes, as the index rows measure the
// index alone: the bytes one delivery allocates, through LMTPData with the
// quota and threads on, grow by at most costNoise a folder message (#2621).
const (
	costSmall = 2000
	costLarge = 10000
	costNoise = 32
)

// costExpectedFail marks the drivers whose delivery still reads something
// whole: the thread file on all three (#2641), maildir's uid list for its
// header, mdbox's map (#2642). A marked driver that starts passing fails, so
// the fix that frees it drops its mark.
var costExpectedFail = map[string]string{
	"maildir": "#2641",
	"sdbox":   "#2641",
	"mdbox":   "#2641 and #2642",
}

type costDriver struct {
	name string
	new  func() mailbox.MailboxBackend
}

var costDrivers = []costDriver{
	{"maildir", func() mailbox.MailboxBackend { return maildir.New(maildir.WithFsync(mailbox.FsyncNever)) }},
	{"sdbox", func() mailbox.MailboxBackend { return dboxv2.New(dboxv2.WithFsync(mailbox.FsyncNever)) }},
	{"mdbox", func() mailbox.MailboxBackend {
		return mdbox.New(mdbox.WithFsync(mailbox.FsyncNever), mdbox.WithLogRotation(32<<10, 1<<20, 0))
	}},
}

func costMessage(i int) string {
	refs := ""
	if i%3 != 0 {
		refs = fmt.Sprintf("In-Reply-To: <m%d@x>\r\n", i-i%3)
	}
	return fmt.Sprintf("Message-ID: <m%d@x>\r\n%sSubject: s %d\r\n\r\n%s\r\n", i, refs, i, strings.Repeat("body text ", 20))
}

// costOptions folds the journal at 32 KiB with no wait, so a folder filled in
// seconds carries the short tail a folder in use does: what opening costs
// with the tail is the index rows' to bound, not this one's.
func costOptions(d costDriver, resolver *mailbox.Resolver) Options {
	return testOpts(Options{
		Mailbox: d.new(), Index: fileindex.New(fileindex.WithFsync(mailbox.FsyncNever),
			fileindex.WithLogCompaction(32<<10, 1<<20, 0)),
		Resolver: resolver, QuotaEngine: true,
		Threads: threads.NewRecorder(threads.NewCache(time.Minute)),
	})
}

// deliveryCost fills a folder of n through one open mailbox, conversations
// recorded, then makes one delivery as the first of a process: no parse of a
// base cached, a thread cache of its own. It returns what that one allocates.
func deliveryCost(t *testing.T, d costDriver, n int) uint64 {
	t.Helper()
	resolver := &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n", DefaultQuotaRules: []string{"*:storage=10G"}}
	info, err := resolver.UserInfo("alice@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	info.Driver = d.name
	fill := &session{opts: costOptions(d, resolver)}
	box := fill.opts.Mailbox.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	ui := fill.opts.Index.OpenUser(info)
	mbox := mailboxbase.Open(box, ui)
	ops := mailops.NewDelivery(fill.mailopsConfig(), mbox, info)
	for i := range n {
		raw := costMessage(i)
		if _, _, _, err := deliverOne(ops, mbox, "INBOX", strings.NewReader(raw), int64(len(raw)), info.Username, "s@x", nil); err != nil {
			t.Fatalf("%s fill %d: %v", d.name, i, err)
		}
	}
	box.Close() //nolint:errcheck
	ui.Close()  //nolint:errcheck

	fileindex.DropBaseImages()
	// As RCPT TO left it from the userdb: the driver is the user's.
	s := &session{opts: costOptions(d, resolver), from: "s@x", rcpts: []string{info.Username},
		rcptUserInfo: map[string]*mailbox.UserInfo{info.Username: info}}
	raw := costMessage(n)
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	sink := &statusSink{}
	if err := s.LMTPData(strings.NewReader(raw), sink); err != nil {
		t.Fatalf("%s: LMTPData: %v", d.name, err)
	}
	runtime.ReadMemStats(&m1)
	if err := sink.errs[info.Username]; err != nil {
		t.Fatalf("%s: the delivery was refused: %v", d.name, err)
	}
	// Into the folder filled, not one the delivery made for itself.
	check := s.opts.Index.OpenUser(info)
	defer check.Close() //nolint:errcheck
	f, err := check.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if f.Messages != uint32(n+1) {
		t.Fatalf("%s: the folder holds %d messages after the delivery, want %d", d.name, f.Messages, n+1)
	}
	return m1.TotalAlloc - m0.TotalAlloc
}

func TestADeliveryCostsNothingAFolderMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("fills two folders a driver")
	}
	for _, d := range costDrivers {
		t.Run(d.name, func(t *testing.T) {
			small, large := deliveryCost(t, d, costSmall), deliveryCost(t, d, costLarge)
			grow := (float64(large) - float64(small)) / (costLarge - costSmall)
			measured := fmt.Sprintf("one delivery allocates %d B into %d, %d B into %d: %.1f B a message, bound %d",
				small, costSmall, large, costLarge, grow, costNoise)
			why, marked := costExpectedFail[d.name]
			switch ok := grow <= costNoise; {
			case marked && ok:
				t.Errorf("%s now passes (%s): drop its expected-fail mark", d.name, measured)
			case marked:
				t.Logf("%s, expected to fail until %s: %s", d.name, why, measured)
			case !ok:
				t.Errorf("%s: %s", d.name, measured)
			default:
				t.Logf("%s: %s", d.name, measured)
			}
		})
	}
}
