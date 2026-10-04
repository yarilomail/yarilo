package imap_test

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// countingDict counts what a session asks of the annotation dict; every one of
// those is a round trip once the dict lives in a service (#1902).
type countingDict struct {
	dict.Dict
	lookups atomic.Int64
}

func (d *countingDict) Lookup(ctx context.Context, set *dict.OpSettings, key string) ([][]byte, bool, error) {
	d.lookups.Add(1)
	return d.Dict.Lookup(ctx, set, key)
}

func startCostClient(t *testing.T, imapSieve bool) (*imapclient.Client, *countingDict, *atomic.Int64) {
	t.Helper()
	eng := sieve.New(config.SieveConfig{
		Enabled: true, MaxRedirects: 32, MaxActions: 32, MaxScriptSize: 65536,
		DefaultName: "yarilo", ImapSieveEnabled: imapSieve, ImapSieveScriptDir: t.TempDir(),
	}, nil, nil, nil)

	inner, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	md := &countingDict{Dict: inner}
	t.Cleanup(func() { _ = inner.Close() })

	fetches := &atomic.Int64{}
	srv := imapserver.New(imapserver.Options{
		Mailbox:      countingStore{MailboxBackend: maildir.New(), fetches: fetches},
		Index:        file.New(),
		Resolver:     &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"},
		AuthRelay:    authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
		MetadataDict: md,
		SieveEngine:  eng,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatal(err)
	}
	return c, md, fetches
}

func appendCost(t *testing.T, c *imapclient.Client) {
	t.Helper()
	body := "From: a@b\r\nSubject: cost\r\n\r\nbody\r\n"
	cmd := c.Append("INBOX", int64(len(body)), nil)
	if _, err := cmd.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

// With imapsieve off, a stored message costs no annotation lookups at all: the
// event used to ask twice before the engine looked at the switch (#1902).
func TestAStoreAsksNothingWhenImapSieveIsOff(t *testing.T) {
	c, md, _ := startCostClient(t, false)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	before := md.lookups.Load()
	for i := 0; i < 5; i++ {
		appendCost(t, c)
	}
	if got := md.lookups.Load() - before; got != 0 {
		t.Errorf("five appends made %d annotation lookups with imapsieve off, want 0", got)
	}
}

// A STORE with imapsieve off asks nothing either: the switch is checked before
// the command resolves the bound script, not after (#1902).
func TestAStoreCommandAsksNothingWhenImapSieveIsOff(t *testing.T) {
	c, md, _ := startCostClient(t, false)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		appendCost(t, c)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	before := md.lookups.Load()
	seq := imap.SeqSetNum(1, 2, 3, 4, 5)
	if err := c.Store(seq, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if got := md.lookups.Load() - before; got != 0 {
		t.Errorf("a STORE over five messages made %d annotation lookups with imapsieve off, want 0", got)
	}
}

// With imapsieve on and no script bound anywhere, the annotation is asked for
// but the message is not re-read; the lookups stay bounded per event.
func TestAStoreWithNoScriptDoesNotGrowPerMessage(t *testing.T) {
	c, md, fetches := startCostClient(t, true)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	before := md.lookups.Load()
	const n = 5
	for i := 0; i < n; i++ {
		appendCost(t, c)
	}
	got := md.lookups.Load() - before
	if got > 2*n {
		t.Errorf("%d appends made %d lookups, want at most %d", n, got, 2*n)
	}
	if read := fetches.Load(); read != 0 {
		t.Errorf("%d appends re-read the stored message %d times with no script bound, want 0", n, read)
	}
}

// countingStore counts body reads: an event with no script used to read the
// message it had just written (#1902).
type countingStore struct {
	mailbox.MailboxBackend
	fetches *atomic.Int64
}

func (s countingStore) OpenUser(info *mailbox.UserInfo) mailbox.UserMailbox {
	return countingUser{UserMailbox: s.MailboxBackend.OpenUser(info), fetches: s.fetches}
}

type countingUser struct {
	mailbox.UserMailbox
	fetches *atomic.Int64
}

func (u countingUser) Fetch(folder, filename string, altTier bool) (io.ReadCloser, error) {
	u.fetches.Add(1)
	return u.UserMailbox.Fetch(folder, filename, altTier)
}

// failingLookupDict answers every lookup with an error, as a dict service that
// is down or timing out does.
type failingLookupDict struct{ dict.Dict }

func (failingLookupDict) Lookup(context.Context, *dict.OpSettings, string) ([][]byte, bool, error) {
	return nil, false, errors.New("dict is down")
}

// A dict failure must not be read as "no script bound": the message is still
// stored, and the skipped filtering is counted and logged (#1905).
func TestADictFailureIsCountedNotSilent(t *testing.T) {
	inner, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	eng := sieve.New(config.SieveConfig{
		Enabled: true, MaxRedirects: 32, MaxActions: 32, MaxScriptSize: 65536,
		DefaultName: "yarilo", ImapSieveEnabled: true, ImapSieveScriptDir: t.TempDir(),
	}, nil, nil, nil)

	srv := imapserver.New(imapserver.Options{
		Mailbox:      maildir.New(),
		Index:        file.New(),
		Resolver:     &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"},
		AuthRelay:    authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
		MetadataDict: failingLookupDict{inner},
		SieveEngine:  eng,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	before := imapserver.ImapSieveLookupErrors()
	appendCost(t, c) // the message is accepted, which is the point

	// Both lookups fail, and both are counted: an error on the mailbox-bound
	// annotation must not be masked by "not found" on the server-wide one.
	if got := imapserver.ImapSieveLookupErrors() - before; got != 2 {
		t.Errorf("a failed annotation lookup was counted %v times, want 2 (mailbox and server-wide)", got)
	}

	// And the mail is still there: a dict outage does not refuse delivery.
	sel, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.NumMessages == 0 {
		t.Error("the message was not stored, so a dict outage refused mail")
	}
}
