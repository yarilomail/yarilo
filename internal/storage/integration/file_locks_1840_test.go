package integration_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Two sessions storing flags on different messages never wait for each other:
// a flag write is a rename, and a rename excludes nobody (#1840).
func TestTwoSessionsStoreWithoutWaiting(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home}
	a := maildir.New().OpenUser(info)
	b := maildir.New().OpenUser(info)
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		body := "From: a@b\r\n\r\n" + strconv.Itoa(i) + "\r\n"
		saved, _, _, err := a.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		named, err := mailbox.Driver(a).(mailbox.UIDNamer).AssignUID("INBOX", saved, uint32(i+1))
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, named)
	}

	before := fileLockHolds(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, box := range []mailbox.UserMailbox{a, b} {
		wg.Add(1)
		go func(i int, box mailbox.UserMailbox) {
			defer wg.Done()
			_, errs[i] = mailbox.Driver(box).(mailbox.FlagWriter).WriteFlags("INBOX", names[i], []string{`\Seen`}, nil)
		}(i, box)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if got := fileLockHolds(t) - before; got != 0 {
		t.Errorf("two stores took %d file locks, want 0", got)
	}
}

// Two processes appending at once keep every row: without the hold over
// read-change-write both read the same list and one row is lost (#1840).
func TestTwoProcessAppendsKeepEveryRow(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u2@example.com", Home: home}

	const perProc = 12
	type proc struct {
		mb  mailbox.UserMailbox
		idx mailbox.UserIndex
	}
	procs := make([]proc, 2)
	for i := range procs {
		mb := maildir.New().OpenUser(info)
		if err := mb.Init(); err != nil {
			t.Fatal(err)
		}
		procs[i] = proc{mb: mb, idx: file.New().OpenUser(info)}
		t.Cleanup(func() { _ = procs[i].idx.Close() })
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2*perProc)
	for _, p := range procs {
		folder, err := p.idx.OpenFolder("INBOX", 1)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(p proc, folderID uint64) {
			defer wg.Done()
			for k := 0; k < perProc; k++ {
				body := "From: a@b\r\n\r\nx\r\n"
				saved, vsize, guid, serr := p.mb.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
				if serr != nil {
					errs <- serr
					return
				}
				m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
				if rerr := mailboxbase.RecordSaved(p.idx, p.mb, folderID, "INBOX", saved, m); rerr != nil {
					errs <- rerr
					return
				}
			}
		}(p, folder.ID)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("appender: %v", err)
	}

	rows, uids := readUIDListRows(t, filepath.Join(home, "Maildir", "yarilo-uidlist"))
	if rows != 2*perProc {
		t.Errorf("the list names %d messages, want %d", rows, 2*perProc)
	}
	if len(uids) != rows {
		t.Errorf("the list holds %d rows under %d distinct uids", rows, len(uids))
	}
}

// Two cycles committing into one journal each leave a whole group: a reader
// counts exactly the boundaries that were written, never a torn one (#1840).
func TestTwoCommitsLeaveWholeGroups(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u3@example.com", Home: home}
	idxA := file.New().OpenUser(info)
	idxB := file.New().OpenUser(info)
	t.Cleanup(func() { _ = idxA.Close(); _ = idxB.Close() })

	fA, err := idxA.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	fB, err := idxB.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for _, c := range []struct {
		idx      mailbox.UserIndex
		folderID uint64
	}{{idxA, fA.ID}, {idxB, fB.ID}} {
		wg.Add(1)
		go func(idx mailbox.UserIndex, id uint64) {
			defer wg.Done()
			for k := 0; k < 8; k++ {
				if aerr := idx.AllocateAndAppend(id, &mailbox.MessageMeta{Size: 10, VSize: 10}); aerr != nil {
					t.Error(aerr)
					return
				}
			}
		}(c.idx, c.folderID)
	}
	wg.Wait()

	// Every byte of the log is inside a boundary's span: a group that
	// interleaved with another leaves a span pointing past its own records.
	if err := boundariesTile(findLog(t, home)); err != nil {
		t.Error(err)
	}
}

// findLog locates the folder's journal below home.
func findLog(t *testing.T, home string) string {
	t.Helper()
	var found string
	if err := filepath.Walk(home, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasSuffix(p, "yarilo.index.log") {
			found = p
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatal("no journal was written below " + home)
	}
	return found
}

// boundariesTile walks the log by the size each BOUNDARY declares and reports
// the first group that does not end where the next one starts.
func boundariesTile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	const hdrLen = 40 // log header, then records
	pos := hdrLen
	groups := 0
	for pos+12 <= len(raw) {
		size := int(binary.LittleEndian.Uint32(raw[pos+8 : pos+12]))
		if size < 12 || pos+size > len(raw) {
			return errTorn(groups, pos, size, len(raw))
		}
		pos += size
		groups++
	}
	if pos != len(raw) {
		return errTorn(groups, pos, 0, len(raw))
	}
	if groups < 16 {
		return errTooFew(groups)
	}
	return nil
}

type tornLog struct{ groups, pos, size, total int }

func (e *tornLog) Error() string {
	return "group " + strconv.Itoa(e.groups) + " at " + strconv.Itoa(e.pos) +
		" declares " + strconv.Itoa(e.size) + " bytes of " + strconv.Itoa(e.total) + ": the log does not tile"
}

func errTorn(groups, pos, size, total int) error {
	return &tornLog{groups: groups, pos: pos, size: size, total: total}
}

type tooFewGroups struct{ groups int }

func (e *tooFewGroups) Error() string {
	return "the log holds " + strconv.Itoa(e.groups) + " groups: the appends did not reach it"
}

func errTooFew(groups int) error { return &tooFewGroups{groups: groups} }

// readUIDListRows returns how many records the list names and how many distinct
// uids they carry.
func readUIDListRows(t *testing.T, path string) (int, map[string]bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	uids := map[string]bool{}
	rows := 0
	for i, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if i == 0 || line == "" {
			continue
		}
		rows++
		uids[strings.Fields(line)[0]] = true
	}
	return rows, uids
}

// fileLockHolds sums every file lock either half has taken.
func fileLockHolds(t *testing.T) int {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, f := range fams {
		if f.GetName() != "fileindex_lock_acquired_total" && f.GetName() != "maildir_lock_acquired_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return int(total)
}

// A delivery lands while a session of the same user has the folder open and
// reaches it: nothing waits on a lease the other process took (#1840, #1841).
func TestADeliveryReachesAnOpenSession(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u4@example.com", Home: home}

	session := maildir.New().OpenUser(info)
	if err := session.Init(); err != nil {
		t.Fatal(err)
	}
	sidx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = sidx.Close() })
	sbox := mailboxbase.Open(session, sidx)
	f, err := sbox.Folder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	// The delivering process: its own store and its own index over the same
	// files, as an LMTP container is beside an IMAP one.
	delivery := maildir.New().OpenUser(info)
	didx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = didx.Close() })
	dbox := mailboxbase.Open(delivery, didx)
	df, err := dbox.Folder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: a@b\r\nSubject: delivered\r\n\r\nx\r\n"
	saved, vsize, guid, serr := delivery.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if serr != nil {
		t.Fatal(serr)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if rerr := mailboxbase.RecordSaved(didx, delivery, df.ID, "INBOX", saved, m); rerr != nil {
		t.Fatal(rerr)
	}

	f, err = sbox.Folder("INBOX", f.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	msgs, gerr := sidx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if len(msgs) != 1 {
		t.Fatalf("the session sees %d messages after a delivery, want 1", len(msgs))
	}
}
