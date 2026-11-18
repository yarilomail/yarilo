package mailcrypt

import (
	"bytes"
	"crypto/ecdh"
	"fmt"
	"strings"
	"testing"

	"filippo.io/age"
)

func sealTo(t *testing.T, plain []byte, to ...age.Recipient) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := New(Options{Recipients: to}).Seal(&out, bytes.NewReader(plain)); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestProbe(t *testing.T) {
	oldID, _ := age.GenerateX25519Identity()
	newID, _ := age.GenerateX25519Identity()
	hybridID, _ := age.GenerateHybridIdentity()
	secondID, _ := age.GenerateX25519Identity()
	strangerID, _ := age.GenerateX25519Identity()
	ref, refKey, _ := referenceVector(t)
	plain := []byte("Subject: x\r\n\r\nbody\r\n")

	rotating := Options{ // public_key is the new key; the old one still reads
		Recipients: []age.Recipient{newID.Recipient()},
		Identities: []age.Identity{oldID, newID},
	}
	two := Options{ // two current recipients
		Recipients: []age.Recipient{newID.Recipient(), secondID.Recipient()},
		Identities: []age.Identity{newID, secondID},
	}
	hybrid := Options{ // the hybrid key current, the X25519 one old
		Recipients: []age.Recipient{hybridID.Recipient()},
		Identities: []age.Identity{oldID, hybridID},
	}
	cases := []struct {
		name   string
		o      Options
		stored []byte
		want   State
	}{
		{"plain", rotating, plain, StatePlain},
		{"new alone", rotating, sealTo(t, plain, newID.Recipient()), StateCurrent},
		{"old alone", rotating, sealTo(t, plain, oldID.Recipient()), StateOtherKey},
		{"old and new together", rotating, sealTo(t, plain, oldID.Recipient(), newID.Recipient()), StateOtherKey},
		{"new and an unknown key", rotating, sealTo(t, plain, newID.Recipient(), strangerID.Recipient()), StateOtherKey},
		{"unknown key only", rotating, sealTo(t, plain, strangerID.Recipient()), StateNoKey},
		{"two current, both", two, sealTo(t, plain, newID.Recipient(), secondID.Recipient()), StateCurrent},
		{"two current, one of them", two, sealTo(t, plain, newID.Recipient()), StateOtherKey},
		{"hybrid current", hybrid, sealTo(t, plain, hybridID.Recipient()), StateCurrent},
		{"X25519 under a hybrid public_key", hybrid, sealTo(t, plain, oldID.Recipient()), StateOtherKey},
		{"reference with its key", Options{ReferenceKeys: []*ecdh.PrivateKey{refKey}}, ref, StateReference},
		{"reference without its key", rotating, ref, StateNoKey},
		{"age with no identity configured", Options{}, sealTo(t, plain, newID.Recipient()), StateNoKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := New(c.o).Probe(newBody(c.stored))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("state %v, want %v", got, c.want)
			}
		})
	}
}

// Probe reads the header, not the body: a 1 MiB message costs a few KB.
func TestProbeReadsOnlyTheHeader(t *testing.T) {
	l, id := ageLayer(t)
	stored := sealTo(t, bytes.Repeat([]byte("a"), 1<<20), id.Recipient())
	b := newBody(stored)
	if st, err := l.Probe(b); err != nil || st != StateCurrent {
		t.Fatalf("state %v err %v", st, err)
	}
	if n := b.read.Load(); n > 8<<10 {
		t.Fatalf("read %d bytes", n)
	}
}

// A current header whose MAC was altered is an error, not a current body.
func TestProbeChecksTheHeaderMAC(t *testing.T) {
	l, id := ageLayer(t)
	stored := sealTo(t, []byte("x\r\n"), id.Recipient())
	at := bytes.Index(stored, []byte("\n--- ")) + 5
	stored[at] ^= 1
	if st, err := l.Probe(newBody(stored)); err == nil {
		t.Fatalf("state %v, no error", st)
	}
}

func TestKeysShowPublicHalvesOnly(t *testing.T) {
	oldID, _ := age.GenerateX25519Identity()
	newID, _ := age.GenerateHybridIdentity()
	lone, _ := age.GenerateHybridIdentity()
	_, refKey, _ := referenceVector(t)
	r := New(Options{
		Recipients:    []age.Recipient{newID.Recipient(), lone.Recipient()},
		Identities:    []age.Identity{oldID, newID},
		ReferenceKeys: []*ecdh.PrivateKey{refKey},
	}).Keys()
	want := []KeyLine{
		{Recipient: oldID.Recipient().String(), Type: "x25519", Current: false},
		{Recipient: newID.Recipient().String(), Type: "hybrid", Current: true},
	}
	if len(r.Identities) != 2 || r.Identities[0] != want[0] || r.Identities[1] != want[1] {
		t.Fatalf("identities %+v", r.Identities)
	}
	if len(r.WriteOnly) != 1 || r.WriteOnly[0] != lone.Recipient().String() {
		t.Fatalf("write-only %v", r.WriteOnly)
	}
	if len(r.ReferenceKeys) != 1 || len(r.ReferenceKeys[0]) != 64 {
		t.Fatalf("reference %v", r.ReferenceKeys)
	}
	all := fmt.Sprintf("%+v", r)
	for _, secret := range []string{oldID.String(), newID.String()} {
		if strings.Contains(all, secret) {
			t.Fatal("a private key is in the report")
		}
	}
}
