package backendapi

import (
	"bytes"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

type rewriteFixture struct {
	ts     *httptest.Server
	root   string
	user   string
	oldID  *age.X25519Identity
	newID  *age.X25519Identity
	server *mailcrypt.Layer
	info   *mailbox.UserInfo
	runAPI func(t *testing.T, kind string, req map[string]any) cryptJobView
}

// sdboxRewriteFixture is an sdbox account under a server whose public key is
// the new one and which still holds the old one.
func sdboxRewriteFixture(t *testing.T) *rewriteFixture {
	t.Helper()
	oldID, _ := age.GenerateX25519Identity()
	newID, _ := age.GenerateX25519Identity()
	server := mailcrypt.New(mailcrypt.Options{
		Recipients: []age.Recipient{newID.Recipient()},
		Identities: []age.Identity{oldID, newID},
	})
	ts, root := storageTestServer(t, func(o *Options) {
		o.Crypt = server
		o.Mailbox = mailbox.Validating(dboxv2.New(dboxv2.WithCrypt(server)), mailbox.DefaultNameRules())
	})
	const user = "alice@example.com"
	materialiseHome(t, ts, user)
	f := &rewriteFixture{ts: ts, root: root, user: user, oldID: oldID, newID: newID, server: server,
		info: &mailbox.UserInfo{Username: user, Home: maildirHome(root, user)}}
	f.runAPI = func(t *testing.T, kind string, req map[string]any) cryptJobView {
		t.Helper()
		req["user"] = user
		if st, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/"+kind, "", req); st != http.StatusAccepted {
			t.Fatalf("start %s: %d %s", kind, st, body)
		}
		return pollCryptJob(t, ts.URL, user)
	}
	return f
}

func (f *rewriteFixture) save(t *testing.T, to []age.Recipient, body string) {
	t.Helper()
	saveWith(t, dboxv2.New(dboxv2.WithCrypt(mailcrypt.New(mailcrypt.Options{Recipients: to}))), f.info, body)
}

// bodies reads every INBOX message through the server's layer, as a session would.
func (f *rewriteFixture) bodies(t *testing.T) map[uint32]string {
	t.Helper()
	box := dboxv2.New(dboxv2.WithCrypt(f.server)).OpenUser(f.info)
	defer box.Close() //nolint:errcheck
	idx := file.New().OpenUser(f.info)
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint32]string{}
	for _, m := range msgs {
		rc, err := box.(mailbox.UIDAddressable).OpenRecord("INBOX", m)
		if err != nil {
			out[m.UID] = "unreadable: " + err.Error()
			continue
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		out[m.UID] = string(b)
	}
	return out
}

func sameBodies(t *testing.T, before, after map[uint32]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%d messages became %d", len(before), len(after))
	}
	for uid, b := range before {
		if after[uid] != b {
			t.Errorf("uid %d: body changed", uid)
		}
	}
}

func TestReencryptSdboxMixedFolder(t *testing.T) {
	f := sdboxRewriteFixture(t)
	f.save(t, nil, "Subject: plain\r\n\r\none\r\n")
	f.save(t, []age.Recipient{f.oldID.Recipient()}, "Subject: old\r\n\r\ntwo\r\n")
	f.save(t, []age.Recipient{f.oldID.Recipient(), f.newID.Recipient()}, "Subject: both\r\n\r\nthree\r\n")
	f.save(t, []age.Recipient{f.newID.Recipient()}, "Subject: new\r\n\r\nfour\r\n")
	before := f.bodies(t)

	job := f.runAPI(t, "reencrypt", map[string]any{})
	if job.State != "done" || job.Rewritten != 3 || job.Failed != 0 {
		t.Fatalf("job %+v", job)
	}
	sameBodies(t, before, f.bodies(t))
	if st := f.runAPI(t, "status", map[string]any{}); st.Totals["current"] != 4 {
		t.Fatalf("after the run: %v", st.Totals)
	}
	if again := f.runAPI(t, "reencrypt", map[string]any{}); again.Rewritten != 0 || again.Failed != 0 {
		t.Fatalf("a second run rewrote %d (failed %d)", again.Rewritten, again.Failed)
	}

	back := f.runAPI(t, "reencrypt", map[string]any{"to": "plain"})
	if back.Rewritten != 4 || back.Failed != 0 {
		t.Fatalf("to plain: %+v", back)
	}
	sameBodies(t, before, f.bodies(t))
	if st := f.runAPI(t, "status", map[string]any{}); st.Totals["plain"] != 4 {
		t.Fatalf("after to plain: %v", st.Totals)
	}
}

// A message no configured key opens is counted and left byte for byte.
func TestReencryptLeavesAMessageWithoutKey(t *testing.T) {
	f := sdboxRewriteFixture(t)
	stranger, _ := age.GenerateX25519Identity()
	f.save(t, []age.Recipient{stranger.Recipient()}, "Subject: s\r\n\r\nx\r\n")
	path := sdboxFile(t, f.info, 1)
	before, _ := os.ReadFile(path)
	job := f.runAPI(t, "reencrypt", map[string]any{})
	if job.NoKey != 1 || job.Rewritten != 0 || job.Failed != 0 {
		t.Fatalf("job %+v", job)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("the message without a key changed")
	}
}

// A new body whose plaintext does not hash the same is never put in place.
func TestReencryptVerifyFailureKeepsTheMessage(t *testing.T) {
	f := sdboxRewriteFixture(t)
	f.save(t, nil, "Subject: s\r\n\r\nx\r\n")
	path := sdboxFile(t, f.info, 1)
	before, _ := os.ReadFile(path)
	verifyHook = func(h hash.Hash) { h.Write([]byte("corrupt")) }
	t.Cleanup(func() { verifyHook = nil })
	job := f.runAPI(t, "reencrypt", map[string]any{})
	if job.Failed != 1 || job.Rewritten != 0 {
		t.Fatalf("job %+v", job)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("a body that failed verification replaced the message")
	}
}

// Delivery into the folder being re-encrypted loses nothing and needs no
// rewrite: new mail is sealed to the current key.
func TestReencryptAlongsideDelivery(t *testing.T) {
	f := sdboxRewriteFixture(t)
	for range 40 {
		f.save(t, []age.Recipient{f.oldID.Recipient()}, "Subject: old\r\n\r\nx\r\n")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 40 {
			f.save(t, []age.Recipient{f.newID.Recipient()}, "Subject: new\r\n\r\ny\r\n")
		}
	}()
	job := f.runAPI(t, "reencrypt", map[string]any{})
	wg.Wait()
	if job.Failed != 0 || job.FoldersFailed != 0 {
		t.Fatalf("job %+v", job)
	}
	all := f.bodies(t)
	if len(all) != 80 {
		t.Fatalf("%d messages, want 80", len(all))
	}
	for uid, b := range all {
		if b != "Subject: old\r\n\r\nx\r\n" && b != "Subject: new\r\n\r\ny\r\n" {
			t.Fatalf("uid %d: %q", uid, b)
		}
	}
	if st := f.runAPI(t, "status", map[string]any{}); st.Totals["current"] != 80 {
		t.Fatalf("after the run: %v", st.Totals)
	}
}

// A home that exists and cannot be read fails the run; it is not an empty one.
func TestReencryptUnreadableHomeFailsTheRun(t *testing.T) {
	f := sdboxRewriteFixture(t)
	f.save(t, nil, "Subject: s\r\n\r\nx\r\n")
	if err := os.Chmod(f.info.Home, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.info.Home, 0o700) })
	if st, body := doJSON(t, f.ts, http.MethodPost, "/api/backend/crypt/reencrypt", "",
		map[string]any{"user": f.user}); st != http.StatusAccepted {
		t.Fatalf("start: %d %s", st, body)
	}
	job := waitCryptJob(t, f.ts.URL, f.user)
	if job.State != "failed" {
		t.Fatalf("job %+v: an unreadable home reported as %s", job, job.State)
	}
}

func sdboxFile(t *testing.T, info *mailbox.UserInfo, uid uint32) string {
	t.Helper()
	return filepath.Join(info.Home, "sdbox", "mailboxes", "INBOX", "dbox-Mails", fmt.Sprintf("u.%d", uid))
}
