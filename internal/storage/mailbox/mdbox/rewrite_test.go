package mdbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	agelib "filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

type rewriteKeys struct {
	oldID, newID *agelib.X25519Identity
	server       *mailcrypt.Layer
}

func newRewriteKeys(t *testing.T) rewriteKeys {
	t.Helper()
	oldID, _ := agelib.GenerateX25519Identity()
	newID, _ := agelib.GenerateX25519Identity()
	return rewriteKeys{oldID, newID, mailcrypt.New(mailcrypt.Options{
		Recipients: []agelib.Recipient{newID.Recipient()},
		Identities: []agelib.Identity{oldID, newID},
	})}
}

// lockedMdbox opens alice's store against a lock service, sealing with l.
func lockedMdbox(t *testing.T, home, alt string, locker locks.Locker, l *mailcrypt.Layer) *userMailbox {
	t.Helper()
	info := &mailbox.UserInfo{Username: "alice@example.com", Home: home}
	if alt != "" {
		info.AltDir = alt
	}
	u := New(WithLocker(locker), WithCrypt(l)).OpenUser(info).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	return u
}

// storeRewrite seals every body that is not current, the job's rule.
func storeRewrite(l *mailcrypt.Layer) mailbox.StoreRewrite {
	return mailbox.StoreRewrite{
		Needs: func(b mailbox.RawBody) (bool, error) {
			st, err := l.Probe(b)
			return err == nil && st != mailcrypt.StateCurrent && st != mailcrypt.StateNoKey, err
		},
		Body: mailbox.BodyRewrite{
			Fill: func(src mailbox.RawBody, dst io.Writer) (int64, bool, error) {
				o, err := l.Open(src, nil)
				if err != nil {
					return 0, false, err
				}
				defer o.Close() //nolint:errcheck
				var plain bytes.Buffer
				if _, err := io.Copy(&plain, o); err != nil {
					return 0, false, err
				}
				n := int64(plain.Len())
				return n, true, l.Seal(dst, &plain)
			},
			Verify: func(mailbox.RawBody) error { return nil },
		},
	}
}

// saveVia saves through u with its layer swapped for one sealing to "to", as
// a server with other keys would have written the message.
func saveVia(t *testing.T, u *userMailbox, to []agelib.Recipient, body string) string {
	t.Helper()
	prev := u.b.crypt
	u.b.crypt = mailcrypt.New(mailcrypt.Options{Recipients: to}).ForDriver(driverName)
	defer func() { u.b.crypt = prev }()
	name, _, _, err := u.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func bodyOf(t *testing.T, u *userMailbox, name string) string {
	t.Helper()
	rc, err := u.Fetch("INBOX", name, false)
	if err != nil {
		t.Fatalf("fetch %s: %v", name, err)
	}
	defer rc.Close() //nolint:errcheck
	b, _ := io.ReadAll(rc)
	return string(b)
}

func storeFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "m.") {
			out = append(out, e.Name())
		}
	}
	return out
}

// Plain, old-key and old+new records, one of them expunged and not purged,
// all become current; bodies, GUIDs, V and refcounts stay; the old file goes.
func TestRewriteStoreMixed(t *testing.T) {
	k := newRewriteKeys(t)
	u := lockedMdbox(t, t.TempDir(), "", lockstest.Server(t)(), k.server)
	names := []string{
		saveVia(t, u, nil, "Subject: plain\r\n\r\none\r\n"),
		saveVia(t, u, []agelib.Recipient{k.oldID.Recipient()}, "Subject: old\r\n\r\ntwo\r\n"),
		saveVia(t, u, []agelib.Recipient{k.oldID.Recipient(), k.newID.Recipient()}, "Subject: both\r\n\r\nthree\r\n"),
		saveVia(t, u, []agelib.Recipient{k.oldID.Recipient()}, "Subject: gone\r\n\r\nfour\r\n"),
	}
	bodies := map[string]string{}
	for _, n := range names {
		bodies[n] = bodyOf(t, u, n)
	}
	if err := u.Remove("INBOX", names[3]); err != nil {
		t.Fatal(err)
	}
	m, _ := u.openMap()
	before, _ := m.RecordsReloaded()
	byUID := map[uint32]uint16{}
	guids := map[uint32][16]byte{}
	for _, e := range before {
		byUID[e.UID], guids[e.UID] = e.RefCount, e.GUID
	}
	oldFiles := storeFiles(t, u.storagePath())

	var rewritten int
	rw := storeRewrite(k.server)
	rw.File = func(f mailbox.FileRewrite) {
		if f.Err != nil {
			t.Fatalf("file %d: %v", f.FileID, f.Err)
		}
		rewritten += f.Rewritten
	}
	if err := u.RewriteStore(rw); err != nil {
		t.Fatal(err)
	}
	if rewritten != 4 {
		t.Fatalf("rewrote %d, want 4 (the expunged one included)", rewritten)
	}
	after, _ := m.RecordsReloaded()
	if len(after) != len(before) {
		t.Fatalf("%d records became %d", len(before), len(after))
	}
	for _, e := range after {
		if e.RefCount != byUID[e.UID] || e.GUID != guids[e.UID] {
			t.Errorf("map_uid %d: refcount %d guid %x, was %d %x", e.UID, e.RefCount, e.GUID, byUID[e.UID], guids[e.UID])
		}
		raw, err := u.rawEntry(e, false)
		if err != nil {
			t.Fatal(err)
		}
		st, err := k.server.Probe(raw)
		_ = raw.Close()
		if err != nil || st != mailcrypt.StateCurrent {
			t.Errorf("map_uid %d: %v %v", e.UID, st, err)
		}
	}
	for _, n := range names[:3] {
		if got := bodyOf(t, u, n); got != bodies[n] {
			t.Errorf("%s: body %q, want %q", n, got, bodies[n])
		}
	}
	for _, f := range oldFiles {
		if _, err := os.Stat(filepath.Join(u.storagePath(), f)); err == nil {
			t.Errorf("%s is still there", f)
		}
	}
	// A second pass touches nothing.
	again := 0
	rw.File = func(f mailbox.FileRewrite) { again += f.Rewritten }
	if err := u.RewriteStore(rw); err != nil || again != 0 {
		t.Fatalf("second pass rewrote %d, err %v", again, err)
	}
}

// The reference's bare-LF records keep their bytes and V.
func TestRewriteStoreKeepsBareLF(t *testing.T) {
	k := newRewriteKeys(t)
	u := lockedMdbox(t, t.TempDir(), "", lockstest.Server(t)(), k.server)
	if err := os.WriteFile(u.mfilePath(1), dboxref.MdboxFile(t), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ := u.openMap()
	if _, err := m.AppendRecord(1, 4690, 0, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	src, _ := os.Open(u.mfilePath(1))
	oldBody, oldTr, err := readRecordRaw(src, 4690)
	_ = src.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := u.RewriteStore(storeRewrite(k.server)); err != nil {
		t.Fatal(err)
	}
	recs, _ := m.RecordsReloaded()
	f, _ := os.Open(u.mfilePath(recs[0].FileID))
	defer f.Close() //nolint:errcheck
	body, tr, err := readRecordRaw(f, recs[0].Offset)
	if err != nil {
		t.Fatal(err)
	}
	o, err := k.server.Open(rawSection{SectionReader: io.NewSectionReader(bytes.NewReader(body), 0, int64(len(body)))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(o)
	if !bytes.Equal(plain, oldBody) {
		t.Fatalf("plaintext %q, want the stored %q", plain, oldBody)
	}
	if tr.vsize != oldTr.vsize || tr.guid != oldTr.guid || !tr.internalDate.Equal(oldTr.internalDate) {
		t.Fatalf("trailer %+v, was %+v", tr, oldTr)
	}
}

// A purge between the holds moves the records: the rewrite leaves the file,
// removes its own output, and every body still reads.
func TestRewriteStoreGivesWayToAPurge(t *testing.T) {
	k := newRewriteKeys(t)
	u := lockedMdbox(t, t.TempDir(), "", lockstest.Server(t)(), k.server)
	keep := saveVia(t, u, []agelib.Recipient{k.oldID.Recipient()}, "Subject: keep\r\n\r\nx\r\n")
	drop := saveVia(t, u, []agelib.Recipient{k.oldID.Recipient()}, "Subject: drop\r\n\r\ny\r\n")
	want := bodyOf(t, u, keep)
	if err := u.Remove("INBOX", drop); err != nil {
		t.Fatal(err)
	}
	testBetweenRewriteHolds = func() {
		if _, err := u.Purge(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { testBetweenRewriteHolds = nil })
	var outcome error
	rw := storeRewrite(k.server)
	rw.File = func(f mailbox.FileRewrite) { outcome = f.Err }
	if err := u.RewriteStore(rw); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(outcome, mailbox.ErrRecordChanged) {
		t.Fatalf("outcome %v", outcome)
	}
	if got := bodyOf(t, u, keep); got != want {
		t.Fatalf("body %q", got)
	}
	m, _ := u.openMap()
	recs, _ := m.RecordsReloaded()
	live := map[string]bool{}
	for _, e := range recs {
		live["m."+strconv.FormatUint(uint64(e.FileID), 10)] = true
	}
	for _, f := range storeFiles(t, u.storagePath()) {
		if !live[f] {
			t.Errorf("%s names no record: the rewrite's output was left", f)
		}
	}
}

// A body that fails verification leaves the file and the map as they were.
func TestRewriteStoreVerifyFailure(t *testing.T) {
	k := newRewriteKeys(t)
	u := lockedMdbox(t, t.TempDir(), "", lockstest.Server(t)(), k.server)
	saveVia(t, u, nil, "Subject: s\r\n\r\nx\r\n")
	before, _ := os.ReadFile(u.mfilePath(1))
	m, _ := u.openMap()
	recsBefore, _ := m.RecordsReloaded()
	rw := storeRewrite(k.server)
	rw.Body.Verify = func(mailbox.RawBody) error { return errors.New("hash differs") }
	var outcome error
	rw.File = func(f mailbox.FileRewrite) { outcome = f.Err }
	if err := u.RewriteStore(rw); err != nil {
		t.Fatal(err)
	}
	if outcome == nil {
		t.Fatal("no failure reported")
	}
	if after, _ := os.ReadFile(u.mfilePath(1)); !bytes.Equal(before, after) {
		t.Fatal("the file changed")
	}
	recsAfter, _ := m.RecordsReloaded()
	if !sameRecords(recsBefore, recsAfter) {
		t.Fatal("the map changed")
	}
	if got := storeFiles(t, u.storagePath()); len(got) != 1 {
		t.Fatalf("files %v", got)
	}
}

func TestRewriteStoreRefusesWithoutTheLockService(t *testing.T) {
	home := t.TempDir()
	u := New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}
	if err := u.RewriteStore(storeRewrite(newRewriteKeys(t).server)); err == nil {
		t.Fatal("a rewrite ran with no lock service")
	}
}

// The map is repointed while this process holds the map lock, and a save
// between the holds lands in the fresh append target, not in the output.
func TestRewriteStoreHoldsTheMapAndKeepsSavesOut(t *testing.T) {
	k := newRewriteKeys(t)
	dial := lockstest.Server(t)
	u := lockedMdbox(t, t.TempDir(), "", dial(), k.server)
	other := dial()
	saveVia(t, u, nil, "Subject: s\r\n\r\nx\r\n")
	var savedName string
	testBetweenRewriteHolds = func() { savedName = saveVia(t, u, nil, "Subject: during\r\n\r\ny\r\n") }
	held := false
	testBeforeRewriteMove = func() {
		ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), "test-other-pod"), time.Second)
		defer cancel()
		_, err := other.Lock(ctx, locks.MdboxMapKey("alice@example.com"), locks.Owner("alice@example.com", "another-pod"), 5*time.Second)
		held = errors.Is(err, locks.ErrBusy)
	}
	t.Cleanup(func() { testBetweenRewriteHolds, testBeforeRewriteMove = nil, nil })
	var out mailbox.FileRewrite
	rw := storeRewrite(k.server)
	rw.File = func(f mailbox.FileRewrite) { out = f }
	if err := u.RewriteStore(rw); err != nil {
		t.Fatal(err)
	}
	if out.Err != nil || out.Rewritten != 1 {
		t.Fatalf("outcome %+v", out)
	}
	if !held {
		t.Fatal("another process could take the map lock while the map was repointed")
	}
	m, _ := u.openMap()
	recs, _ := m.RecordsReloaded()
	perFile := map[uint32]int{}
	for _, e := range recs {
		perFile[e.FileID]++
	}
	for id, n := range perFile {
		if n != 1 {
			t.Fatalf("m.%d holds %d records: the save landed in the output (%v)", id, n, perFile)
		}
	}
	if got := bodyOf(t, u, savedName); got != "Subject: during\r\n\r\ny\r\n" {
		t.Fatalf("the save during the rewrite reads %q", got)
	}
}

// A file in alt is rewritten into alt.
func TestRewriteStoreStaysInItsTier(t *testing.T) {
	k := newRewriteKeys(t)
	home, alt := t.TempDir(), t.TempDir()
	u := lockedMdbox(t, home, alt, lockstest.Server(t)(), k.server)
	name := saveVia(t, u, nil, "Subject: s\r\n\r\nx\r\n")
	if _, err := u.AltMove(AltMoveQuery{}); err != nil {
		t.Fatal(err)
	}
	if got := storeFiles(t, u.storagePath()); len(got) != 0 {
		t.Fatalf("primary still holds %v", got)
	}
	if err := u.RewriteStore(storeRewrite(k.server)); err != nil {
		t.Fatal(err)
	}
	if got := storeFiles(t, u.storagePath()); len(got) != 0 {
		t.Fatalf("the rewrite moved mail into primary: %v", got)
	}
	if got := storeFiles(t, u.altStoragePath()); len(got) != 1 {
		t.Fatalf("alt holds %v", got)
	}
	if got := bodyOf(t, u, name); got != "Subject: s\r\n\r\nx\r\n" {
		t.Fatalf("body %q", got)
	}
}

// Once the map is repointed the output is never removed: an old file that
// will not go stays as a leftover the next pass sweeps, and an error after
// the repoint keeps the output the map names. Every body still reads.
func TestRewriteStoreKeepsTheOutputAfterTheRepoint(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T)
		fails bool
	}{
		{"the old file cannot be unlinked", func(t *testing.T) {
			removeOldFile = func(string) error { return &os.PathError{Op: "remove", Err: os.ErrPermission} }
			t.Cleanup(func() { removeOldFile = os.Remove })
		}, false},
		{"an error after the map write", func(t *testing.T) {
			testAfterRewriteMove = func() error { return errors.New("the lease was lost on the way out") }
			t.Cleanup(func() { testAfterRewriteMove = nil })
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newRewriteKeys(t)
			u := lockedMdbox(t, t.TempDir(), "", lockstest.Server(t)(), k.server)
			names := []string{
				saveVia(t, u, nil, "Subject: a\r\n\r\none\r\n"),
				saveVia(t, u, []agelib.Recipient{k.oldID.Recipient()}, "Subject: b\r\n\r\ntwo\r\n"),
			}
			want := map[string]string{}
			for _, n := range names {
				want[n] = bodyOf(t, u, n)
			}
			c.setup(t)
			err := u.RewriteStore(storeRewrite(k.server))
			if c.fails != (err != nil) {
				t.Fatalf("err %v", err)
			}
			m, _ := u.openMap()
			if _, err := m.RecordsReloaded(); err != nil {
				t.Fatal(err)
			}
			for _, n := range names {
				if got := bodyOf(t, u, n); got != want[n] {
					t.Fatalf("%s reads %q after the repoint", n, got)
				}
			}
		})
	}
}

// What a pass killed between its holds leaves, a file no record names, is
// swept by the next pass once it is old enough; a young one stays.
func TestRewriteStoreSweepsLeftovers(t *testing.T) {
	k := newRewriteKeys(t)
	u := lockedMdbox(t, t.TempDir(), "", lockstest.Server(t)(), k.server)
	name := saveVia(t, u, nil, "Subject: a\r\n\r\none\r\n")
	file, _ := os.ReadFile(u.mfilePath(1))
	old, young := u.mfilePath(50), u.mfilePath(51)
	for _, p := range []string{old, young} {
		if err := os.WriteFile(p, file, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(old, past, past)
	rw := storeRewrite(k.server)
	rw.LeftoverAge = time.Minute
	if err := u.RewriteStore(rw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("the old leftover is still there")
	}
	if _, err := os.Stat(young); err != nil {
		t.Error("a young file no record names was taken")
	}
	if got := bodyOf(t, u, name); got != "Subject: a\r\n\r\none\r\n" {
		t.Fatalf("body %q", got)
	}
}

// A leftover carrying copies of live records is not brought back by a
// rebuild that restores orphans: its records' GUIDs are the live copies',
// which the scan pairs with the map first, so no message appears twice.
func TestALeftoverIsNotRestoredTwice(t *testing.T) {
	box, idx := newBoxAndIndex(t, filepath.Join(t.TempDir(), "home"))
	deliverMsg(t, box, idx, "INBOX", "Subject: a\r\n\r\none\r\n")
	deliverMsg(t, box, idx, "INBOX", "Subject: b\r\n\r\ntwo\r\n")
	file, err := os.ReadFile(box.mfilePath(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(box.mfilePath(77), file, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := box.RebuildStorage(mailboxbase.Open(box, idx), idx, true); err != nil {
		t.Fatal(err)
	}
	f, _ := idx.OpenFolder("INBOX", 0)
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("INBOX holds %d messages after the rebuild, want the 2 delivered", len(msgs))
	}
	seen := map[[16]byte]bool{}
	for _, m := range msgs {
		if seen[m.GUID] {
			t.Fatalf("GUID %x twice", m.GUID)
		}
		seen[m.GUID] = true
	}
}
