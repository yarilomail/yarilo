package jmap

import (
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/userstate/threads"
)

func (ts *threadedServer) merge(t *testing.T) {
	t.Helper()
	if err := threads.MergeStore(ts.path, func(fn func() error) error { return fn() }); err != nil {
		t.Fatalf("merge: %v", err)
	}
}

// After the log is merged into its base, Thread/changes from a state of this
// generation reports what deliveries since then did: a reply into a thread
// the base holds is updated, a new conversation created, and the new state is
// the one Thread/get gives (#2649).
func TestThreadChangesAfterAMergeReportsWhatFollowed(t *testing.T) {
	ts := newThreadedServer(t)
	var roots []string
	for i := 0; i < 20; i++ {
		roots = append(roots, ts.deliver(t, fmt.Sprintf("Message-ID: <r%d@x>\r\nSubject: Root %d\r\n\r\nbody\r\n", i, i)))
	}
	ts.merge(t)
	since := threadStateOf(t, ts)

	ts.deliver(t, "Message-ID: <reply@x>\r\nIn-Reply-To: <r3@x>\r\nSubject: Re: Root 3\r\n\r\nbody\r\n")
	fresh := ts.deliver(t, "Message-ID: <new@x>\r\nSubject: Unrelated\r\n\r\nbody\r\n")

	payload := ts.call(t, "Thread/changes", fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, since))
	if errType, _ := payload["type"].(string); errType != "" {
		t.Fatalf("Thread/changes refused: %v", payload)
	}
	if got := idList(t, payload, "created"); len(got) != 1 || got[0] != fresh {
		t.Errorf("created = %v, want the new conversation %q", got, fresh)
	}
	if got := idList(t, payload, "updated"); len(got) != 1 || got[0] != roots[3] {
		t.Errorf("updated = %v, want the conversation the base holds %q", got, roots[3])
	}
	if now := threadStateOf(t, ts); payload["newState"] != now {
		t.Errorf("newState %v, Thread/get says %q", payload["newState"], now)
	}
}

// A state from before the merge names positions the merge folded away: the
// client is told to start again, not that nothing changed.
func TestThreadChangesFromBeforeAMergeCannotBeCalculated(t *testing.T) {
	ts := newThreadedServer(t)
	for i := 0; i < 20; i++ {
		ts.deliver(t, fmt.Sprintf("Message-ID: <r%d@x>\r\nSubject: Root %d\r\n\r\nbody\r\n", i, i))
	}
	since := threadStateOf(t, ts)
	ts.merge(t)
	ts.deliver(t, "Message-ID: <reply@x>\r\nIn-Reply-To: <r3@x>\r\nSubject: Re: Root 3\r\n\r\nbody\r\n")
	payload, errType := changesCall(t, ts.srv, "Thread/changes", fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, since))
	if errType != "cannotCalculateChanges" {
		t.Errorf("a state from before the merge answered %q/%v, want cannotCalculateChanges", errType, payload)
	}
}
