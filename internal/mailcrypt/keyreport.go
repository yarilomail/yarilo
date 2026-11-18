package mailcrypt

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"

	"filippo.io/age"
)

// KeyReport describes the configured keys by their public halves only.
type KeyReport struct {
	// Identities are the private keys' recipients; Current marks those in
	// public_key, which new mail is sealed to.
	Identities []KeyLine `json:"identities"`
	// WriteOnly are public_key recipients no configured private key opens.
	WriteOnly []string `json:"write_only"`
	// ReferenceKeys are SHA-256 fingerprints of reference_keys' public keys.
	ReferenceKeys []string `json:"reference_keys"`
}

// KeyLine is one private key, named by its recipient.
type KeyLine struct {
	Recipient string `json:"recipient"`
	Type      string `json:"type"`
	Current   bool   `json:"current"`
}

// Keys reports the layer's keys; nothing in it is secret.
func (l *Layer) Keys() KeyReport {
	r := KeyReport{Identities: []KeyLine{}, WriteOnly: []string{}, ReferenceKeys: []string{}}
	var want []string
	for _, rc := range l.opts().Recipients {
		if s, ok := recipientString(rc); ok {
			want = append(want, s)
		}
	}
	var have []string
	for _, id := range l.opts().ids {
		s, ok := identityRecipient(id)
		if !ok {
			continue
		}
		have = append(have, s)
		typ := "x25519"
		if _, hybrid := id.(*age.HybridIdentity); hybrid {
			typ = "hybrid"
		}
		r.Identities = append(r.Identities, KeyLine{Recipient: s, Type: typ, Current: slices.Contains(want, s)})
	}
	for _, s := range want {
		if !slices.Contains(have, s) {
			r.WriteOnly = append(r.WriteOnly, s)
		}
	}
	for _, k := range l.opts().ReferenceKeys {
		sum := sha256.Sum256(k.PublicKey().Bytes())
		r.ReferenceKeys = append(r.ReferenceKeys, hex.EncodeToString(sum[:]))
	}
	return r
}
