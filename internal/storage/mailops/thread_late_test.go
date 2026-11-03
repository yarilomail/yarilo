package mailops

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/internal/userstate/threads"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// gate is a locker whose thread lock can be refused, as a lock held past the
// wait refuses it.
type gate struct {
	lockstest.Recorder
	refuse bool
	held   bool
}

func (g *gate) Lock(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	if g.refuse {
		return locks.Lock{}, errors.New("locks: resource is busy")
	}
	g.held = true
	return g.Recorder.Lock(ctx, resource, owner, ttl)
}

func (g *gate) Unlock(ctx context.Context, token string) error {
	g.held = false
	return g.Recorder.Unlock(ctx, token)
}

// openWatch counts the message opens made while the thread lock is held.
type openWatch struct {
	mailbox.Box
	lock      *gate
	underLock int
}

func (w *openWatch) OpenMessage(folder string, m *mailbox.MessageMeta) (io.ReadCloser, error) {
	if w.lock.held {
		w.underLock++
	}
	return w.Box.OpenMessage(folder, m)
}

type threadAccount struct {
	o    *Ops
	box  mailbox.Box
	lock *gate
	path string
}

func newThreadAccount(t *testing.T) *threadAccount {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir(), Driver: "maildir"}
	store := maildir.New().OpenUser(info)
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	box := mailboxbase.Open(store, idx)
	g := &gate{}
	cfg := Config{Protocol: "test", Threads: threads.NewRecorder(nil), Locker: g}
	return &threadAccount{o: New(cfg, box, info), box: box, lock: g, path: threads.PathFor(info)}
}

func (a *threadAccount) save(t *testing.T, headers string) *mailbox.MessageMeta {
	t.Helper()
	body := headers + "\r\n\r\nbody\r\n"
	s, err := a.o.Save(a.box, "INBOX", nil, SaveRequest{Body: strings.NewReader(body), Size: int64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	return s.Meta
}

func (a *threadAccount) threadOf(t *testing.T, m *mailbox.MessageMeta) (string, bool) {
	t.Helper()
	st, err := threads.Load(a.path)
	if err != nil {
		t.Fatal(err)
	}
	return st.ThreadOfGUID(mailbox.FormatObjectID(m.GUID))
}

// A save the thread lock turns away stays saved and outside its conversation;
// a read that cannot take the lock either leaves it so without an error; the
// first read that can records it where it belongs, and a later reply to it
// alone joins the same conversation (#2420).
func TestAMessageLeftOutIsThreadedOnTheFirstRead(t *testing.T) {
	a := newThreadAccount(t)
	parent := a.save(t, "Message-ID: <a@x>\r\nSubject: s")
	thread, ok := a.threadOf(t, parent)
	if !ok {
		t.Fatal("the first message was not threaded")
	}

	failed := testutil.ToFloat64(metricThreadRecordFailed)
	a.lock.refuse = true
	reply := a.save(t, "Message-ID: <b@x>\r\nIn-Reply-To: <a@x>\r\nSubject: Re: s")
	if _, ok := a.threadOf(t, reply); ok {
		t.Fatal("a save the lock turned away was threaded anyway")
	}
	if got := testutil.ToFloat64(metricThreadRecordFailed) - failed; got != 1 {
		t.Errorf("save_not_recorded_total grew by %v, want 1", got)
	}

	late, err := a.o.ThreadLate("INBOX", []*mailbox.MessageMeta{reply})
	if err == nil || len(late) != 0 {
		t.Errorf("a read under a refused lock recorded %v, err %v; want nothing and the refusal", late, err)
	}
	if _, ok := a.threadOf(t, reply); ok {
		t.Fatal("a read that could not take the lock wrote the sidecar")
	}

	recorded := testutil.ToFloat64(metricThreadLate)
	a.lock.refuse = false
	late, err = a.o.ThreadLate("INBOX", []*mailbox.MessageMeta{reply})
	if err != nil {
		t.Fatal(err)
	}
	if got := late[mailbox.FormatObjectID(reply.GUID)]; got != thread {
		t.Errorf("the read recorded the reply in %q, want its parent's %q", got, thread)
	}
	if got, _ := a.threadOf(t, reply); got != thread {
		t.Errorf("the sidecar has the reply in %q, want %q", got, thread)
	}
	if got := testutil.ToFloat64(metricThreadLate) - recorded; got != 1 {
		t.Errorf("recorded_on_read_total grew by %v, want 1", got)
	}

	// Its Message-ID is known now, so a reply to it alone finds the conversation.
	next := a.save(t, "Message-ID: <c@x>\r\nIn-Reply-To: <b@x>\r\nSubject: Re: s")
	if got, _ := a.threadOf(t, next); got != thread {
		t.Errorf("a reply to the late message went to %q, want %q", got, thread)
	}

	// Once recorded, a read finds it and records nothing more.
	recorded = testutil.ToFloat64(metricThreadLate)
	if _, err := a.o.ThreadLate("INBOX", []*mailbox.MessageMeta{reply}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(metricThreadLate) - recorded; got != 0 {
		t.Errorf("a second read recorded the message again (%v)", got)
	}
}

// An account the migration has not reached has no sidecar, and a read does not
// start building one message at a time (#1425).
func TestAReadDoesNotStartASidecar(t *testing.T) {
	a := newThreadAccount(t)
	a.lock.refuse = true
	m := a.save(t, "Message-ID: <a@x>\r\nSubject: s")
	a.lock.refuse = false
	if late, err := a.o.ThreadLate("INBOX", []*mailbox.MessageMeta{m}); err != nil || len(late) != 0 {
		t.Errorf("a read on an account with no sidecar recorded %v, err %v", late, err)
	}
	if _, err := threads.Load(a.path); err == nil {
		if _, ok := a.threadOf(t, m); ok {
			t.Error("a read started the sidecar")
		}
	}
}

// The headers are read before the lock: under it a read only appends, so the
// disk never sits in the queue that left the messages out (#2420).
func TestALateRecordReadsNoMessageUnderTheLock(t *testing.T) {
	a := newThreadAccount(t)
	a.save(t, "Message-ID: <a@x>\r\nSubject: s")
	a.lock.refuse = true
	var missed []*mailbox.MessageMeta
	for _, id := range []string{"b", "c", "d"} {
		missed = append(missed, a.save(t, "Message-ID: <"+id+"@x>\r\nIn-Reply-To: <a@x>\r\nSubject: Re: s"))
	}
	a.lock.refuse = false
	w := &openWatch{Box: a.box, lock: a.lock}
	o := New(a.o.cfg, w, a.o.user)
	late, err := o.ThreadLate("INBOX", missed)
	if err != nil || len(late) != len(missed) {
		t.Fatalf("recorded %d of %d, err %v", len(late), len(missed), err)
	}
	if w.underLock != 0 {
		t.Errorf("%d messages were opened while the thread lock was held", w.underLock)
	}
}
