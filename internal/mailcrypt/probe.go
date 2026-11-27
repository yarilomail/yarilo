package mailcrypt

import (
	"errors"
	"fmt"
	"io"
	"slices"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

// State is what a stored body is with respect to the configured keys.
type State int

const (
	StatePlain     State = iota
	StateCurrent         // age, sealed to exactly the recipients of public_key
	StateOtherKey        // age that another configured identity opens, alone or beside the current ones
	StateReference       // the reference format, opened by reference_keys
	StateNoKey           // encrypted, and no configured key opens it
)

// States lists every State in the order reports print them.
var States = []State{StatePlain, StateCurrent, StateOtherKey, StateReference, StateNoKey}

func (s State) String() string {
	switch s {
	case StatePlain:
		return "plain"
	case StateCurrent:
		return "current"
	case StateOtherKey:
		return "other_key"
	case StateReference:
		return "reference"
	case StateNoKey:
		return "no_key"
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// Probe classifies body from its header alone; nothing past the header is
// decrypted, and the key cache is neither read nor filled.
func (l *Layer) Probe(body Body) (State, error) {
	head := make([]byte, PrefixLen)
	n, err := body.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("mailcrypt/probe: %w", err)
	}
	switch Classify(head[:n]) {
	case Age:
		return l.probeAge(body)
	case Reference:
		_, err := refstream.NewReader(io.NewSectionReader(body, 0, body.Size()), l.opts().ReferenceKeys...)
		switch {
		case err == nil:
			return StateReference, nil
		case errors.Is(err, refstream.ErrNoKey):
			return StateNoKey, nil
		}
		return 0, fmt.Errorf("mailcrypt/probe: %w", err)
	}
	return StatePlain, nil
}

func (l *Layer) probeAge(body Body) (State, error) {
	hdr, err := age.ExtractHeader(io.NewSectionReader(body, 0, body.Size()))
	if err != nil {
		return 0, fmt.Errorf("mailcrypt/probe: %w", err)
	}
	rec := &stanzaRecorder{}
	_, _ = age.DecryptHeader(hdr, rec)
	current := l.currentIdentities()
	ids := l.opts().ids
	var byCurrent, byOther, byNone int
	for _, st := range rec.stanzas {
		one := []*age.Stanza{st}
		switch {
		case opensWithAny(current, one):
			byCurrent++
		case opensWithAny(ids, one):
			byOther++
		default:
			byNone++
		}
	}
	switch {
	case byCurrent == 0 && byOther == 0:
		return StateNoKey, nil
	case byOther > 0 || byNone > 0 || byCurrent != len(l.opts().Recipients):
		return StateOtherKey, nil
	}
	// The header's MAC, which the per-stanza unwraps do not check.
	if _, err := age.DecryptHeader(hdr, current...); err != nil {
		return 0, fmt.Errorf("mailcrypt/probe: %w", err)
	}
	return StateCurrent, nil
}

func opensWithAny(ids []age.Identity, stanzas []*age.Stanza) bool {
	for _, id := range ids {
		if _, err := id.Unwrap(stanzas); err == nil {
			return true
		}
	}
	return false
}

// currentIdentities are the identities whose recipient is in public_key.
func (l *Layer) currentIdentities() []age.Identity {
	want := make([]string, 0, len(l.opts().Recipients))
	for _, r := range l.opts().Recipients {
		if s, ok := recipientString(r); ok {
			want = append(want, s)
		}
	}
	var out []age.Identity
	for _, id := range l.opts().ids {
		if s, ok := identityRecipient(id); ok && slices.Contains(want, s) {
			out = append(out, id)
		}
	}
	return out
}

// stanzaRecorder is an identity that opens nothing and keeps the stanzas age
// hands it, which age does not otherwise expose.
type stanzaRecorder struct{ stanzas []*age.Stanza }

func (r *stanzaRecorder) Unwrap(stanzas []*age.Stanza) ([]byte, error) {
	r.stanzas = stanzas
	return nil, age.ErrIncorrectIdentity
}

func recipientString(r age.Recipient) (string, bool) {
	s, ok := r.(fmt.Stringer)
	if !ok {
		return "", false
	}
	return s.String(), true
}

func identityRecipient(id age.Identity) (string, bool) {
	switch v := id.(type) {
	case *age.X25519Identity:
		return v.Recipient().String(), true
	case *age.HybridIdentity:
		return v.Recipient().String(), true
	}
	return "", false
}
