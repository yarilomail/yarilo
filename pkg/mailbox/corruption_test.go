package mailbox

import (
	"errors"
	"testing"
)

// Both embed the interface so they satisfy UserMailbox without carrying it: a
// method the gating is meant to stop short of panics instead of answering.
type fakeHealer struct{ UserMailbox }

func (fakeHealer) HealCorruptFolder(Box, *Folder) ([]uint32, error) { return nil, nil }

type plainBox struct{ UserMailbox }

func TestCanReactiveHeal(t *testing.T) {
	if !CanReactiveHeal(fakeHealer{}) {
		t.Error("a ReactiveHealer must qualify")
	}
	if CanReactiveHeal(plainBox{}) {
		t.Error("a driver without HealCorruptFolder must not qualify")
	}
	if CanReactiveHeal(nil) {
		t.Error("nil must not qualify")
	}
}

// stubBox answers Store and nothing else: the embedded interface is nil, so any
// method the gating does not stop short of panics.
type stubBox struct {
	Box
	store UserMailbox
}

func (b stubBox) Store() UserMailbox { return b.store }

// TestMarkCorruptOnFetchErrGating verifies the marker is gated: every no-op path
// must return before it reaches the index. Reaching the end is the assertion.
func TestMarkCorruptOnFetchErrGating(t *testing.T) {
	// The store cannot heal → must not mark (would otherwise strand a folder FSCKD).
	MarkCorruptOnFetchErr(stubBox{store: plainBox{}}, "INBOX", ErrCorruptStorage)
	// err is nil → no-op.
	MarkCorruptOnFetchErr(stubBox{store: fakeHealer{}}, "INBOX", nil)
	// err is not corruption → no-op (transient I/O must not mark).
	MarkCorruptOnFetchErr(stubBox{store: fakeHealer{}}, "INBOX", errors.New("input/output error"))
}
