package maildir

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/pkg/filelock"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// copyRewrite writes the old bytes back unchanged behind a marker the test
// can see on disk, so a swap is observable without a key.
func markRewrite(mark string) mailbox.BodyRewrite {
	return mailbox.BodyRewrite{
		Fill: func(src mailbox.RawBody, dst io.Writer) (int64, bool, error) {
			b, err := io.ReadAll(io.NewSectionReader(src, 0, src.Size()))
			if err != nil {
				return 0, false, err
			}
			if !bytes.HasPrefix(b, []byte(mark)) {
				b = append([]byte(mark), b...)
			}
			_, err = dst.Write(b)
			return int64(len(b)), false, err
		},
		Verify: func(mailbox.RawBody) error { return nil },
	}
}

// two handles on one home, as two processes open it.
func twoHandles(t *testing.T) (*userMailbox, *userMailbox) {
	t.Helper()
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: testHome(root, "u@x.com")}
	a := New().OpenUser(info).(*userMailbox)
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	b := New().OpenUser(info).(*userMailbox)
	return a, b
}

func saveFlagged(t *testing.T, box *userMailbox, folder, body string, uid uint32) string {
	t.Helper()
	name, _, _, err := box.Save(folder, strings.NewReader(body), uid, int64(len(body)), []string{`\Seen`}, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	published, err := box.AssignUID(folder, name, uid)
	if err != nil {
		t.Fatal(err)
	}
	return published
}

func filesOf(t *testing.T, box *userMailbox, folder string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, sub := range []string{"cur", "new"} {
		entries, _ := os.ReadDir(filepath.Join(box.folderPath(folder), sub))
		for _, e := range entries {
			out[maildirBase(e.Name())] = append(out[maildirBase(e.Name())], e.Name())
		}
	}
	return out
}

func noRewriteTemps(t *testing.T, box *userMailbox, folder string) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(box.folderPath(folder), "tmp"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), rewritePrefix) {
			t.Fatalf("a rewrite temp was left in %s: %s", folder, e.Name())
		}
	}
}

// The file keeps its name, so S= and W=, and its date; a bare-LF body is
// written back as the bytes that lay on disk.
func TestRewriteKeepsNameDateAndBytes(t *testing.T) {
	box, _ := twoHandles(t)
	body := "From: a@b\n\nhello\n"
	name := saveFlagged(t, box, "INBOX", body, 1)
	path, _ := box.locate("INBOX", name)
	lastYear := time.Now().AddDate(-1, 0, 0)
	if err := os.Chtimes(path, lastYear, lastYear); err != nil {
		t.Fatal(err)
	}
	if err := box.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: 1}, markRewrite("X-Mark: 1\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file left its name: %v", err)
	}
	if string(got) != "X-Mark: 1\n"+body {
		t.Fatalf("body %q", got)
	}
	if st, _ := os.Stat(path); !st.ModTime().Equal(lastYear) {
		t.Fatalf("dated %v, want %v", st.ModTime(), lastYear)
	}
	noRewriteTemps(t, box, "INBOX")
}

// What another process does between the read and the swap: a STORE is
// followed onto the new name, and an expunge, an imapsieve unlink or a move
// out leave no file behind.
func TestRewriteAgainstAnotherProcess(t *testing.T) {
	cases := []struct {
		name     string
		act      func(t *testing.T, other *userMailbox, name string, recorded *bool)
		wantErr  error
		wantFile bool
	}{
		{"STORE renamed it", func(t *testing.T, other *userMailbox, name string, _ *bool) {
			if _, err := other.WriteFlags("INBOX", name, []string{`\Seen`, `\Flagged`}, nil); err != nil {
				t.Fatal(err)
			}
		}, nil, true},
		{"expunged, record first", func(t *testing.T, other *userMailbox, name string, recorded *bool) {
			*recorded = false
			if err := other.Remove("INBOX", name); err != nil {
				t.Fatal(err)
			}
		}, mailbox.ErrExpunged, false},
		{"expunged, record gone and file still there", func(_ *testing.T, _ *userMailbox, _ string, recorded *bool) {
			*recorded = false
		}, mailbox.ErrExpunged, true},
		// The file went first and the index still has the record: changed, not
		// expunged, until the index says so; a rerun finds it gone.
		{"imapsieve, file first", func(t *testing.T, other *userMailbox, name string, _ *bool) {
			if err := other.Remove("INBOX", name); err != nil {
				t.Fatal(err)
			}
		}, mailbox.ErrRecordChanged, false},
		{"moved out", func(t *testing.T, other *userMailbox, name string, _ *bool) {
			if _, _, err := other.Move("INBOX", "Other", name, [16]byte{}); err != nil {
				t.Fatal(err)
			}
		}, mailbox.ErrRecordChanged, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			box, other := twoHandles(t)
			if err := box.Create("Other"); err != nil {
				t.Fatal(err)
			}
			name := saveFlagged(t, box, "INBOX", "Subject: s\r\n\r\nx\r\n", 1)
			recorded := true
			rw := markRewrite("X-Mark: 1\r\n")
			rw.Recorded = func() (bool, error) { return recorded, nil }
			testAfterRewriteVerified = func() { c.act(t, other, name, &recorded) }
			t.Cleanup(func() { testAfterRewriteVerified = nil })

			err := box.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: 1}, rw)
			if c.wantErr == nil && err != nil || c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("err %v, want %v", err, c.wantErr)
			}
			files := filesOf(t, box, "INBOX")
			if len(files) > 1 || len(files[maildirBase(name)]) > 1 {
				t.Fatalf("files %v", files)
			}
			if have := len(files) == 1; have != c.wantFile {
				t.Fatalf("a file in INBOX: %v, want %v (%v)", have, c.wantFile, files)
			}
			if c.name == "STORE renamed it" {
				only := files[maildirBase(name)][0]
				if !strings.Contains(only, "F") {
					t.Fatalf("the swap undid the STORE: %s", only)
				}
				path, _ := box.locate("INBOX", only)
				if b, _ := os.ReadFile(path); !bytes.HasPrefix(b, []byte("X-Mark: 1")) {
					t.Fatal("the new body did not land")
				}
			}
			noRewriteTemps(t, box, "INBOX")
		})
	}
}

// The rename runs while the folder's crypt lock is held exclusive: another
// process's rename cannot take it shared at that moment.
func TestRewriteRenamesUnderTheCryptLock(t *testing.T) {
	box, other := twoHandles(t)
	saveFlagged(t, box, "INBOX", "Subject: s\r\n\r\nx\r\n", 1)
	prev := uidListLockWait
	uidListLockWait = 50 * time.Millisecond
	t.Cleanup(func() { uidListLockWait = prev })
	var takeErr error
	testBeforeRewriteRename = func() {
		release, err := other.sharedCrypt("INBOX", "test-other-process")
		if err == nil {
			release()
		}
		takeErr = err
	}
	t.Cleanup(func() { testBeforeRewriteRename = nil })
	if err := box.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: 1}, markRewrite("X: 1\r\n")); err != nil {
		t.Fatal(err)
	}
	if takeErr == nil {
		t.Fatal("another process took the crypt lock while the rename ran")
	}
}

// The writers a swap races take the crypt lock shared: a swap holding it
// exclusive from another handle keeps a STORE's rename, an unlink, an
// expunge, a move out and an arrival's move from new/ waiting.
func TestWritersTakeTheCryptLock(t *testing.T) {
	prev := uidListLockWait
	uidListLockWait = 50 * time.Millisecond
	t.Cleanup(func() { uidListLockWait = prev })
	for _, c := range []struct {
		name string
		act  func(box *userMailbox, name string) error
	}{
		{"STORE", func(box *userMailbox, name string) error {
			res := box.WriteFlagsMulti("INBOX", []mailbox.FlagWrite{{UID: 1, Filename: name, Flags: []string{`\Flagged`}}})
			return res[0].Err
		}},
		{"remove", func(box *userMailbox, name string) error { return box.Remove("INBOX", name) }},
		{"expunge hold", func(box *userMailbox, _ string) error {
			return box.HoldFolder("INBOX", "expunge", func() error { return nil })
		}},
		{"move out", func(box *userMailbox, name string) error {
			_, _, err := box.Move("INBOX", "Other", name, [16]byte{})
			return err
		}},
		{"restore of a move", func(box *userMailbox, name string) error {
			return box.RestoreMoved("INBOX", name, "Other", name, nil)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			box, other := twoHandles(t)
			if err := box.Create("Other"); err != nil {
				t.Fatal(err)
			}
			name := saveFlagged(t, box, "INBOX", "Subject: s\r\n\r\nx\r\n", 1)
			release, err := other.exclusiveCrypt("INBOX")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if err := c.act(box, name); err == nil {
				t.Fatal("it went ahead while another process's swap held the crypt lock")
			}
			if files := filesOf(t, box, "INBOX"); len(files[maildirBase(name)]) != 1 || files[maildirBase(name)][0] != name {
				t.Fatalf("the file changed: %v", files)
			}
		})
	}
}

// Two sessions renaming in one folder hold the crypt lock together: neither
// waits for the other (#1840 holds).
func TestSessionsShareTheCryptLock(t *testing.T) {
	box, other := twoHandles(t)
	a, err := box.sharedCrypt("INBOX", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer a()
	prev := uidListLockWait
	uidListLockWait = 20 * time.Millisecond
	t.Cleanup(func() { uidListLockWait = prev })
	b, err := other.sharedCrypt("INBOX", "b")
	if err != nil {
		t.Fatalf("a second session waited for the first: %v", err)
	}
	b()
}

// On a volume locked without flock the swap refuses and the sessions' renames
// go on unheld.
func TestRewriteRefusesWithoutFlock(t *testing.T) {
	box, _ := twoHandles(t)
	box.b.lockMethod = filelock.MethodDotlock
	name := saveFlagged(t, box, "INBOX", "Subject: s\r\n\r\nx\r\n", 1)
	path, _ := box.locate("INBOX", name)
	before, _ := os.ReadFile(path)
	err := box.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: 1}, markRewrite("X: 1\r\n"))
	if !errors.Is(err, filelock.ErrNoSharedLock) {
		t.Fatalf("err %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("the file changed")
	}
	if _, err := box.WriteFlags("INBOX", name, []string{`\Flagged`}, nil); err != nil {
		t.Fatalf("a STORE on a dotlock volume: %v", err)
	}
	noRewriteTemps(t, box, "INBOX")
}

func TestANestedTakeOfTheCryptLockIsRefused(t *testing.T) {
	box, _ := twoHandles(t)
	release, err := box.sharedCrypt("INBOX", "outer")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer func() {
		if recover() == nil {
			t.Fatal("a nested take was not refused")
		}
	}()
	_, _ = box.sharedCrypt("INBOX", "inner")
}

func TestANestedTakeOfTheListIsRefused(t *testing.T) {
	box, _ := twoHandles(t)
	release, err := box.holdList("INBOX", "outer")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer func() {
		if recover() == nil {
			t.Fatal("a nested take was not refused")
		}
	}()
	_, _ = box.holdList("INBOX", "inner")
}

// Thousands of rounds of STORE, expunge, imapsieve unlink and move from one
// process while another swaps: never two files for one message, never a file
// for one the index no longer has, and no lock wait that runs out.
func TestRewriteStress(t *testing.T) {
	rounds := 2000
	if testing.Short() {
		rounds = 300
	}
	box, other := twoHandles(t)
	if err := box.Create("Other"); err != nil {
		t.Fatal(err)
	}
	type rec struct {
		folder, name string
		uid          uint32
	}
	var mu sync.Mutex
	index := map[string]rec{} // base -> where the index says it is
	const n = 40
	for i := range n {
		name := saveFlagged(t, box, "INBOX", fmt.Sprintf("Subject: m%d\r\n\r\nx\r\n", i), uint32(i+1))
		index[maildirBase(name)] = rec{"INBOX", name, uint32(i + 1)}
	}
	nextUID := uint32(1000)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // the sessions
		defer wg.Done()
		rng := rand.New(rand.NewPCG(1, 2))
		for range rounds {
			mu.Lock()
			var pick rec
			var base string
			for b, r := range index {
				if r.folder == "INBOX" {
					base, pick = b, r
					break
				}
			}
			mu.Unlock()
			if base == "" {
				continue
			}
			current, _, err := other.currentName("INBOX", base)
			if err != nil {
				errs <- fmt.Errorf("index has %s, disk does not: %w", base, err)
				return
			}
			switch rng.IntN(4) {
			case 0:
				res := other.WriteFlagsMulti("INBOX", []mailbox.FlagWrite{{UID: pick.uid, Filename: current,
					Flags: []string{`\Seen`, []string{`\Flagged`, `\Answered`, `\Draft`}[rng.IntN(3)]}}})
				if res[0].Err != nil {
					errs <- res[0].Err
					return
				}
			case 1: // expunge: the record, then the file
				mu.Lock()
				delete(index, base)
				mu.Unlock()
				if err := other.HoldFolder("INBOX", "expunge", func() error { return other.RemoveHeld("INBOX", current) }); err != nil {
					errs <- err
					return
				}
			case 2: // imapsieve: the file, then the record
				if err := other.Remove("INBOX", current); err != nil {
					errs <- err
					return
				}
				mu.Lock()
				delete(index, base)
				mu.Unlock()
			case 3: // move out, then the source record
				moved, _, err := other.Move("INBOX", "Other", current, [16]byte{})
				if err != nil {
					errs <- err
					return
				}
				nextUID++
				uid := nextUID
				published, err := other.AssignUID("Other", moved, uid)
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				delete(index, base)
				index[maildirBase(published)] = rec{"Other", published, uid}
				mu.Unlock()
			}
			mu.Lock()
			if len(index) < n/2 {
				nextUID++
				uid := nextUID
				mu.Unlock()
				name := saveFlagged(t, other, "INBOX", "Subject: new\r\n\r\ny\r\n", uid)
				mu.Lock()
				index[maildirBase(name)] = rec{"INBOX", name, uid}
			}
			mu.Unlock()
		}
	}()
	go func() { // the re-encryption
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			mu.Lock()
			var bases []string
			for b, r := range index {
				if r.folder == "INBOX" {
					bases = append(bases, b)
				}
			}
			mu.Unlock()
			for _, b := range bases {
				mu.Lock()
				r, ok := index[b]
				mu.Unlock()
				if !ok {
					continue
				}
				rw := markRewrite(fmt.Sprintf("X-Round: %d\r\n", i))
				rw.Recorded = func() (bool, error) {
					mu.Lock()
					defer mu.Unlock()
					got, ok := index[b]
					return ok && got.folder == "INBOX", nil
				}
				err := box.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: r.uid}, rw)
				if err != nil && !errors.Is(err, mailbox.ErrExpunged) && !errors.Is(err, mailbox.ErrRecordChanged) &&
					!errors.Is(err, mailbox.ErrRecordBusy) &&
					!errors.Is(err, os.ErrNotExist) {
					errs <- fmt.Errorf("rewrite %s: %w", b, err)
					return
				}
			}
			if len(bases) == 0 {
				break
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, folder := range []string{"INBOX", "Other"} {
		for base, names := range filesOf(t, box, folder) {
			if len(names) > 1 {
				t.Errorf("%s: two files for one message: %v", folder, names)
			}
			if r, ok := index[base]; !ok || r.folder != folder {
				t.Errorf("%s: %s is on disk and the index does not have it there (brought back)", folder, names)
			}
		}
		noRewriteTemps(t, box, folder)
	}
	for base, r := range index {
		if len(filesOf(t, box, r.folder)[base]) != 1 {
			t.Errorf("the index has %s in %s and the disk does not", base, r.folder)
		}
	}
}

// The reconcile's move from new/ to cur/ renames a message file too, and
// waits for a swap holding the crypt lock; the arrival stays in new/.
func TestTheArrivalMoveTakesTheCryptLock(t *testing.T) {
	box, idx, folder := recSetup(t)
	deliverToNewDir(t, box, "1700006000.M1P1.mda", "Subject: a\r\n\r\nb\r\n")
	other := New().OpenUser(&mailbox.UserInfo{Username: "u@x.com", Home: box.home}).(*userMailbox)
	release, err := other.exclusiveCrypt("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	prev := uidListLockWait
	uidListLockWait = 50 * time.Millisecond
	t.Cleanup(func() { uidListLockWait = prev })
	if _, err := box.ReconcileArrivals(nil, idx, folder); err == nil {
		t.Fatal("the arrival moved while a swap held the crypt lock")
	}
	if _, err := os.Stat(filepath.Join(box.folderPath("INBOX"), "new", "1700006000.M1P1.mda")); err != nil {
		t.Fatalf("the arrival left new/: %v", err)
	}
	release()
	if _, err := box.ReconcileArrivals(nil, idx, folder); err != nil {
		t.Fatalf("after the swap: %v", err)
	}
}

// Sessions storing without pause in one folder do not starve the swap for
// ever: it gives way when busy and still gets through.
func TestRewriteGetsThroughUnderConstantStores(t *testing.T) {
	box, _ := twoHandles(t)
	const n = 20
	var names []string
	for i := range n {
		names = append(names, saveFlagged(t, box, "INBOX", fmt.Sprintf("Subject: m%d\r\n\r\nx\r\n", i), uint32(i+1)))
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for s := range 4 {
		session := New().OpenUser(&mailbox.UserInfo{Username: "u@x.com", Home: box.home}).(*userMailbox)
		wg.Add(1)
		go func() {
			defer wg.Done()
			flags := []string{`\Seen`, `\Flagged`}
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				base := maildirBase(names[(i+s)%n])
				cur, _, err := session.currentName("INBOX", base)
				if err == nil {
					_, _ = session.WriteFlags("INBOX", cur, flags[:1+i%2], nil)
				}
			}
		}()
	}
	done := 0
	for round := 0; round < 20 && done < n; round++ {
		for i := range n {
			err := box.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: uint32(i + 1)}, markRewrite("X-Done: 1\r\n"))
			switch {
			case err == nil:
				done++
			case errors.Is(err, mailbox.ErrRecordBusy), errors.Is(err, mailbox.ErrRecordChanged):
			default:
				close(stop)
				wg.Wait()
				t.Fatal(err)
			}
		}
	}
	close(stop)
	wg.Wait()
	if done == 0 {
		t.Fatal("the swap never got through four sessions' stores")
	}
	for base, files := range filesOf(t, box, "INBOX") {
		if len(files) != 1 {
			t.Fatalf("%s: %v", base, files)
		}
	}
	t.Logf("%d swaps landed against four storing sessions", done)
}

// On a volume with no room the lock file cannot be made, and a STORE, an
// unlink and an expunge still go, unheld and counted; a swap there refuses.
func TestAFullVolumeStillLetsMailGo(t *testing.T) {
	box, _ := twoHandles(t)
	if err := box.Create("Other"); err != nil {
		t.Fatal(err)
	}
	a := saveFlagged(t, box, "INBOX", "Subject: a\r\n\r\nx\r\n", 1)
	b := saveFlagged(t, box, "INBOX", "Subject: b\r\n\r\ny\r\n", 2)
	undo := filelock.SetNoRoomForLockFiles()
	defer undo()
	before := testutil.ToFloat64(metricCryptLock.WithLabelValues("shared", "no-room"))
	if _, err := box.WriteFlags("INBOX", a, []string{`\Deleted`}, nil); err != nil {
		t.Fatalf("STORE: %v", err)
	}
	cur, _, _ := box.currentName("INBOX", maildirBase(a))
	if err := box.HoldFolder("INBOX", "expunge", func() error { return box.RemoveHeld("INBOX", cur) }); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	if err := box.Remove("INBOX", b); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if files := filesOf(t, box, "INBOX"); len(files) != 0 {
		t.Fatalf("left %v", files)
	}
	if got := testutil.ToFloat64(metricCryptLock.WithLabelValues("shared", "no-room")) - before; got != 3 {
		t.Fatalf("counted %v unheld takes, want 3", got)
	}
	c := func() string {
		undo()
		defer func() { undo = filelock.SetNoRoomForLockFiles() }()
		return saveFlagged(t, box, "Other", "Subject: c\r\n\r\nz\r\n", 1)
	}()
	_ = c
	if err := box.RewriteRecord("Other", &mailbox.MessageMeta{UID: 1}, markRewrite("X: 1\r\n")); err == nil {
		t.Fatal("a swap ran where its lock file could not be made")
	}
}

// A file renamed again each time it is looked up is missing from both lookups
// of the raw read; it is expunged only when the index has no record, and
// otherwise changed, which a rerun takes. The file stays as it was.
func TestRewriteTellsARenameFromAnExpunge(t *testing.T) {
	for _, c := range []struct {
		name     string
		recorded bool
		want     error
	}{
		{"still in the index", true, mailbox.ErrRecordChanged},
		{"gone from the index", false, mailbox.ErrExpunged},
	} {
		t.Run(c.name, func(t *testing.T) {
			box, other := twoHandles(t)
			name := saveFlagged(t, box, "INBOX", "Subject: s\r\n\r\nx\r\n", 1)
			flags := [][]string{{`\Seen`, `\Flagged`}, {`\Seen`}}
			i := 0
			testBeforeRawLocate = func() {
				cur, _, err := other.currentName("INBOX", maildirBase(name))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := other.WriteFlags("INBOX", cur, flags[i%2], nil); err != nil {
					t.Fatal(err)
				}
				i++
				box.folderCacheFor("INBOX").invalidateDirEntries("test")
			}
			t.Cleanup(func() { testBeforeRawLocate = nil })
			rw := markRewrite("X: 1\r\n")
			rw.Recorded = func() (bool, error) { return c.recorded, nil }
			err := box.RewriteRecord("INBOX", &mailbox.MessageMeta{UID: 1}, rw)
			if !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
			testBeforeRawLocate = nil
			cur, _, _ := box.currentName("INBOX", maildirBase(name))
			path, _ := box.locate("INBOX", cur)
			if b, _ := os.ReadFile(path); bytes.HasPrefix(b, []byte("X: 1")) {
				t.Fatal("the file was rewritten")
			}
		})
	}
}
