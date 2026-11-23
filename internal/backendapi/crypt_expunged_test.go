package backendapi

import (
	"net/http"
	"testing"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A message another session expunges between the job's list and its read is
// expunged meanwhile: not failed, and the job ends done, on maildir and sdbox.
func TestReencryptCountsAMessageExpungedMeanwhile(t *testing.T) {
	for _, driver := range []string{"maildir", "sdbox"} {
		t.Run(driver, func(t *testing.T) {
			newID, _ := age.GenerateX25519Identity()
			server := mailcrypt.New(mailcrypt.Options{Recipients: []age.Recipient{newID.Recipient()}, Identities: []age.Identity{newID}})
			locker := lockstest.Server(t)()
			var be func() mailbox.MailboxBackend
			ts, root := storageTestServer(t, func(o *Options) {
				o.Crypt = server
				o.Locker = locker
				if driver == "sdbox" {
					o.Mailbox = mailbox.Validating(dboxv2.New(dboxv2.WithCrypt(server), dboxv2.WithLocker(locker)), mailbox.DefaultNameRules())
				}
			})
			const user = "alice@example.com"
			materialiseHome(t, ts, user)
			info := &mailbox.UserInfo{Username: user, Home: maildirHome(root, user)}
			be = func() mailbox.MailboxBackend { return maildir.New() }
			if driver == "sdbox" {
				be = func() mailbox.MailboxBackend { return dboxv2.New() }
			}
			for range 3 {
				saveWith(t, be(), info, "Subject: plain\r\n\r\nbody\r\n")
			}
			gone := 0
			testBeforeCryptRead = func(folder string, m *mailbox.MessageMeta) {
				if m.UID != 2 || gone > 0 {
					return
				}
				gone++
				store := be().OpenUser(info)
				idx := file.New().OpenUser(info)
				defer store.Close() //nolint:errcheck
				defer idx.Close()   //nolint:errcheck
				other := mailboxbase.Open(store, idx, mailboxbase.ReadOnly())
				f, err := idx.OpenFolder(folder, 0)
				if err != nil {
					t.Error(err)
					return
				}
				if err := other.RemoveMessage(folder, m); err != nil {
					t.Error(err)
				}
				if err := idx.ExpungeMessage(f.ID, m.UID); err != nil {
					t.Error(err)
				}
			}
			t.Cleanup(func() { testBeforeCryptRead = nil })

			if st, body := doJSON(t, ts, http.MethodPost, "/api/backend/crypt/reencrypt", "", map[string]any{"user": user}); st != http.StatusAccepted {
				t.Fatalf("start: %d %s", st, body)
			}
			job := pollCryptJob(t, ts.URL, user)
			if gone != 1 {
				t.Fatalf("the seam expunged %d messages, want 1 (job %+v)", gone, job)
			}
			if job.State != "done" || job.Failed != 0 || job.Skipped != 1 || job.Rewritten != 2 {
				t.Fatalf("job %+v, want done, failed 0, expunged meanwhile 1, rewritten 2", job)
			}
		})
	}
}
