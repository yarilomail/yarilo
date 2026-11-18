package backendapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// saveThrough stores one message in INBOX through a maildir sealing with l.
func saveThrough(t *testing.T, root, user string, l *mailcrypt.Layer, body string) {
	t.Helper()
	info := &mailbox.UserInfo{Username: user, Home: maildirHome(root, user)}
	box := maildir.New(maildir.WithCrypt(l)).OpenUser(info)
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

func TestCryptReencryptWritesNothingYet(t *testing.T) {
	ts, _ := storageTestServer(t)
	st, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/reencrypt", "",
		map[string]any{"user": "alice@example.com", "to": "plain"})
	if st != http.StatusNotImplemented {
		t.Fatalf("status %d %s", st, body)
	}
}

type cryptJobView struct {
	State        string         `json:"state"`
	Error        string         `json:"error"`
	Seen         int            `json:"seen"`
	Totals       map[string]int `json:"totals"`
	WouldRewrite int            `json:"would_rewrite"`
	NoKey        int            `json:"no_key"`
	Failed       int            `json:"failed"`
}

func pollCryptJob(t *testing.T, base, user string) cryptJobView {
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
	if v.Error != "" {
		t.Fatalf("job failed: %s", v.Error)
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
