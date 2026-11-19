package backendapi

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// saveThrough stores one message in INBOX through a maildir sealing with l.
func saveThrough(t *testing.T, root, user string, l *mailcrypt.Layer, body string) {
	t.Helper()
	saveInto(t, &mailbox.UserInfo{Username: user, Home: maildirHome(root, user)}, l, body)
}

func saveInto(t *testing.T, info *mailbox.UserInfo, l *mailcrypt.Layer, body string) {
	t.Helper()
	saveWith(t, maildir.New(maildir.WithCrypt(l)), info, body)
}

// saveWith stores one message in INBOX through be and records it in the index.
func saveWith(t *testing.T, be mailbox.MailboxBackend, info *mailbox.UserInfo, body string) {
	t.Helper()
	box := be.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	defer box.Close() //nolint:errcheck
	idx := file.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := idx.AllocateUID(folder.ID)
	if err != nil {
		t.Fatal(err)
	}
	name, vsize, guid, err := box.Save("INBOX", io.NopCloser(bytes.NewBufferString(body)), uid, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	meta := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := mailboxbase.NameSaved(box, "INBOX", name, meta); err != nil {
		t.Fatal(err)
	}
	if err := idx.AppendMessage(folder.ID, meta); err != nil {
		t.Fatal(err)
	}
}

func TestCryptStatusCountsEveryState(t *testing.T) {
	oldID, _ := age.GenerateX25519Identity()
	newID, _ := age.GenerateX25519Identity()
	stranger, _ := age.GenerateX25519Identity()
	server := mailcrypt.New(mailcrypt.Options{
		Recipients: []age.Recipient{newID.Recipient()},
		Identities: []age.Identity{oldID, newID},
	})
	ts, root := storageTestServer(t, func(o *Options) { o.Crypt = server })
	const user = "alice@example.com"
	materialiseHome(t, ts, user)

	sealer := func(r ...age.Recipient) *mailcrypt.Layer { return mailcrypt.New(mailcrypt.Options{Recipients: r}) }
	for _, s := range []struct {
		l *mailcrypt.Layer
		n int
	}{
		{nil, 2},
		{sealer(newID.Recipient()), 3},
		{sealer(oldID.Recipient()), 1},
		{sealer(oldID.Recipient(), newID.Recipient()), 1},
		{sealer(stranger.Recipient()), 1},
	} {
		for range s.n {
			saveThrough(t, root, user, s.l, "Subject: x\r\n\r\nbody\r\n")
		}
	}

	for _, kind := range []string{"status", "reencrypt"} {
		t.Run(kind, func(t *testing.T) {
			req := map[string]any{"user": user}
			if kind == "reencrypt" {
				req["dry_run"] = true
			}
			if st, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/"+kind, "", req); st != http.StatusAccepted {
				t.Fatalf("start: %d %s", st, body)
			}
			job := pollCryptJob(t, ts.URL, user)
			want := map[string]int{"plain": 2, "current": 3, "other_key": 2, "no_key": 1}
			for k, v := range want {
				if job.Totals[k] != v {
					t.Errorf("%s = %d, want %d (totals %v)", k, job.Totals[k], v, job.Totals)
				}
			}
			if job.State != "done" || job.Seen != 8 || job.NoKey != 1 || job.Failed != 0 {
				t.Fatalf("job %+v", job)
			}
			if kind == "reencrypt" && job.WouldRewrite != 4 {
				t.Fatalf("would rewrite %d, want 4: plain 2 and other key 2", job.WouldRewrite)
			}
		})
	}
}

// A maildir account, mixed and with a bare-LF message, re-encrypted, back to
// plain, and a second run that rewrites nothing; bodies read the same and
// keep the bytes that lay on disk.
func TestReencryptMaildir(t *testing.T) {
	oldID, _ := age.GenerateX25519Identity()
	newID, _ := age.GenerateX25519Identity()
	server := mailcrypt.New(mailcrypt.Options{
		Recipients: []age.Recipient{newID.Recipient()},
		Identities: []age.Identity{oldID, newID},
	})
	ts, root := storageTestServer(t, func(o *Options) {
		o.Crypt = server
		o.Locker = lockstest.Server(t)()
	})
	const user = "alice@example.com"
	materialiseHome(t, ts, user)
	sealer := func(r ...age.Recipient) *mailcrypt.Layer { return mailcrypt.New(mailcrypt.Options{Recipients: r}) }
	saveThrough(t, root, user, nil, "Subject: plain\r\n\r\none\r\n")
	saveThrough(t, root, user, nil, "Subject: bare\n\nlf\n")
	saveThrough(t, root, user, sealer(oldID.Recipient()), "Subject: old\r\n\r\ntwo\r\n")
	saveThrough(t, root, user, sealer(newID.Recipient()), "Subject: new\r\n\r\nthree\r\n")
	home := maildirHome(root, user)
	before := maildirPlain(t, server, home)

	run := func(req map[string]any, kind string) cryptJobView {
		req["user"] = user
		if st, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/"+kind, "", req); st != http.StatusAccepted {
			t.Fatalf("start: %d %s", st, body)
		}
		return pollCryptJob(t, ts.URL, user)
	}
	if job := run(map[string]any{}, "reencrypt"); job.Rewritten != 3 || job.Failed != 0 {
		t.Fatalf("to age: %+v", job)
	}
	if got := maildirPlain(t, server, home); !sameMap(before, got) {
		t.Fatalf("bodies changed:\n%v\n%v", before, got)
	}
	if st := run(map[string]any{}, "status"); st.Totals["current"] != 4 {
		t.Fatalf("after: %v", st.Totals)
	}
	if again := run(map[string]any{}, "reencrypt"); again.Rewritten != 0 {
		t.Fatalf("second run rewrote %d", again.Rewritten)
	}
	if back := run(map[string]any{"to": "plain"}, "reencrypt"); back.Rewritten != 4 || back.Failed != 0 {
		t.Fatalf("to plain: %+v", back)
	}
	if got := maildirPlain(t, server, home); !sameMap(before, got) {
		t.Fatalf("bodies changed on the way back")
	}
}

// maildirPlain is every message file's plaintext by name: the name carries
// S= and W=, so an equal map is also unchanged sizes and flags.
func maildirPlain(t *testing.T, l *mailcrypt.Layer, home string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.Contains(p, string(filepath.Separator)+"cur"+string(filepath.Separator)) &&
			!strings.Contains(p, string(filepath.Separator)+"new"+string(filepath.Separator)) {
			return nil
		}
		o, err := l.OpenFile(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		b, _ := io.ReadAll(o)
		_ = o.Close()
		out[d.Name()] = string(b)
		return nil
	})
	if len(out) == 0 {
		t.Fatal("no message files")
	}
	return out
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

type cryptJobView struct {
	State         string         `json:"state"`
	Error         string         `json:"error"`
	Seen          int            `json:"seen"`
	Totals        map[string]int `json:"totals"`
	WouldRewrite  int            `json:"would_rewrite"`
	Rewritten     int            `json:"rewritten"`
	Skipped       int            `json:"skipped"`
	NoKey         int            `json:"no_key"`
	Failed        int            `json:"failed"`
	FoldersFailed int            `json:"folders_failed"`
}

func pollCryptJob(t *testing.T, base, user string) cryptJobView {
	t.Helper()
	v := waitCryptJob(t, base, user)
	if v.Error != "" {
		t.Fatalf("job failed: %s", v.Error)
	}
	return v
}

// waitCryptJob waits for the user's job to end and returns it as it ended.
func waitCryptJob(t *testing.T, base, user string) cryptJobView {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/backend/crypt/job?user="+user+"&wait=10s", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var v cryptJobView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.State == "running" {
		t.Fatal("job still running after the wait")
	}
	return v
}

func TestVirtualNamespaceOwnsNoMessages(t *testing.T) {
	for _, c := range []struct {
		loc  string
		want bool
	}{
		{"virtual:~/virtual", true},
		{" VIRTUAL:/srv/v", true},
		{"mdbox:~/mdbox", false},
		{"", false},
	} {
		if got := isVirtualNamespace(config.NamespaceConfig{Location: c.loc}); got != c.want {
			t.Errorf("%q: %v, want %v", c.loc, got, c.want)
		}
	}
}

func TestCryptKeysCarryNoPrivateKey(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	l := mailcrypt.New(mailcrypt.Options{Recipients: []age.Recipient{id.Recipient()}, Identities: []age.Identity{id}})
	ts, _ := storageTestServer(t, func(o *Options) { o.Crypt = l })
	st, body := doJSON(t, ts, http.MethodGet, "/api/backend/crypt/keys", "", nil)
	if st != http.StatusOK || !bytes.Contains(body, []byte(id.Recipient().String())) {
		t.Fatalf("status %d body %s", st, body)
	}
	if bytes.Contains(body, []byte(id.String())) || bytes.Contains(body, []byte("AGE-SECRET-KEY")) {
		t.Fatal("the private key is in the answer")
	}
}

// A user's second namespace with a store of its own is walked without a
// flag: a zero that left it out would not be a zero.
func TestCryptStatusWalksEveryNamespaceWithAStore(t *testing.T) {
	oldID, _ := age.GenerateX25519Identity()
	newID, _ := age.GenerateX25519Identity()
	server := mailcrypt.New(mailcrypt.Options{
		Recipients: []age.Recipient{newID.Recipient()},
		Identities: []age.Identity{oldID, newID},
	})
	archive := config.NamespaceConfig{Type: "personal", Prefix: "Archive/", Separator: "/", List: "yes", Location: "maildir:%h/archive"}
	ts, root := storageTestServer(t, func(o *Options) {
		o.Crypt = server
		o.Namespaces = append(o.Namespaces, archive)
	})
	const user = "alice@example.com"
	materialiseHome(t, ts, user)
	saveThrough(t, root, user, mailcrypt.New(mailcrypt.Options{Recipients: []age.Recipient{newID.Recipient()}}), "Subject: a\r\n\r\nb\r\n")

	base := &mailbox.UserInfo{Username: user, Home: maildirHome(root, user)}
	loc, _, err := mailbox.ParseLocation(archive.Location, base)
	if err != nil {
		t.Fatal(err)
	}
	info, err := mailbox.NamespaceUserInfo(base, loc, archive.Separator, mailbox.UserStore)
	if err != nil {
		t.Fatal(err)
	}
	saveInto(t, info, mailcrypt.New(mailcrypt.Options{Recipients: []age.Recipient{oldID.Recipient()}}), "Subject: old\r\n\r\nb\r\n")

	for _, c := range []struct {
		ns       string
		current  int
		otherKey int
	}{
		{"", 1, 1},
		{"personal", 1, 0},
		{"archive", 0, 1},
	} {
		t.Run("namespace="+c.ns, func(t *testing.T) {
			if st, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/status", "",
				map[string]any{"user": user, "namespace": c.ns}); st != http.StatusAccepted {
				t.Fatalf("start: %d %s", st, body)
			}
			job := pollCryptJob(t, ts.URL, user)
			if job.Totals["current"] != c.current || job.Totals["other_key"] != c.otherKey {
				t.Fatalf("totals %v, want current %d other_key %d", job.Totals, c.current, c.otherKey)
			}
		})
	}
}

// A folder whose index cannot be read is a row with its error and a failed
// count, never a folder that silently has no messages.
func TestCryptStatusReportsAFolderItCouldNotOpen(t *testing.T) {
	ts, root := storageTestServer(t, func(o *Options) { o.Crypt = mailcrypt.New(mailcrypt.Options{}) })
	const user = "alice@example.com"
	materialiseHome(t, ts, user)
	saveThrough(t, root, user, nil, "Subject: a\r\n\r\nb\r\n")
	if st, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/create", "",
		map[string]any{"user": user, "folder": "Broken"}); st != 200 {
		t.Fatalf("create: %d %s", st, body)
	}
	breakFolderIndex(t, maildirHome(root, user), "Broken")

	if st, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/status", "", map[string]any{"user": user}); st != http.StatusAccepted {
		t.Fatalf("start: %d %s", st, body)
	}
	job := pollCryptJob(t, ts.URL, user)
	if job.FoldersFailed != 1 {
		t.Fatalf("folders failed %d, want 1 (job %+v)", job.FoldersFailed, job)
	}
	if job.Totals["plain"] != 1 {
		t.Fatalf("INBOX not counted: %v", job.Totals)
	}
}

// breakFolderIndex overwrites the folder's index with bytes no index format
// reads, so the folder cannot be opened.
func breakFolderIndex(t *testing.T, home, folder string) {
	t.Helper()
	var idx string
	_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "yarilo.index" && strings.HasSuffix(filepath.Dir(p), "."+folder) {
			idx = p
		}
		return nil
	})
	if idx == "" {
		t.Fatalf("no index for %s under %s", folder, home)
	}
	if err := os.WriteFile(idx, bytes.Repeat([]byte{0xff}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
}

// backend-api opens a public namespace under the store's lock name, and the
// personal one under the user's (#2598).
func TestNamespaceBundleNamesTheSharedStore(t *testing.T) {
	dir := t.TempDir()
	srv := New(Options{
		Mailbox:  mailbox.Validating(maildir.New(), mailbox.DefaultNameRules()),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"},
		Namespaces: []config.NamespaceConfig{
			{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
			{Type: "shared", Prefix: "Public/", Separator: "/", List: "yes", Location: "maildir:" + dir},
		},
	})
	uc, err := srv.openUserContextReadOnly("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	pub, err := uc.ns(srv, "public")
	if err != nil {
		t.Fatal(err)
	}
	if got := pub.info.LockUser(); got != "store:"+dir {
		t.Errorf("public lock name %q, want store:%s", got, dir)
	}
	if got := uc.info.LockUser(); got != "alice@example.com" {
		t.Errorf("personal lock name %q", got)
	}
}
