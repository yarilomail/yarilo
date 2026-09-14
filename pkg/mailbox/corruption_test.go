package mailbox

import "testing"

// Both embed the interface so they satisfy UserMailbox without carrying it: a
// method the gating is meant to stop short of panics instead of answering.
type fakeHealer struct{ UserMailbox }

func (fakeHealer) HealCorruptFolder(Box, UserIndex, *Folder) ([]uint32, error) { return nil, nil }

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
