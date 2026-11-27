package refstream

import (
	"crypto/ecdh"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

var (
	oidECPublicKey = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	oidX448        = asn1.ObjectIdentifier{1, 3, 101, 111}
	oidMLKEM       = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 4}

	curveOIDs = map[ecdh.Curve]asn1.ObjectIdentifier{
		ecdh.P256(): {1, 2, 840, 10045, 3, 1, 7},
		ecdh.P384(): {1, 3, 132, 0, 34},
		ecdh.P521(): {1, 3, 132, 0, 35},
	}
	ellipticOf = map[ecdh.Curve]elliptic.Curve{
		ecdh.P256(): elliptic.P256(),
		ecdh.P384(): elliptic.P384(),
		ecdh.P521(): elliptic.P521(),
	}
)

// ParsePrivateKeyPEM reads a PKCS#8 or SEC 1 private key: P-256, P-384, P-521
// or X25519. X448 and ML-KEM keys are refused with ErrUnsupported.
func ParsePrivateKeyPEM(data []byte) (*ecdh.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("refstream/key: no PEM block")
	}
	switch block.Type {
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("refstream/key: %w", err)
		}
		return k.ECDH()
	case "PRIVATE KEY":
	default:
		return nil, fmt.Errorf("refstream/key: PEM block %q", block.Type)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if name := unsupportedPKCS8(block.Bytes); name != "" {
			return nil, fmt.Errorf("refstream/key: %s keys: %w", name, ErrUnsupported)
		}
		return nil, fmt.Errorf("refstream/key: %w", err)
	}
	if e, ok := k.(interface {
		ECDH() (*ecdh.PrivateKey, error)
	}); ok {
		return e.ECDH()
	}
	if e, ok := k.(*ecdh.PrivateKey); ok {
		return e, nil
	}
	return nil, fmt.Errorf("refstream/key: %T keys: %w", k, ErrUnsupported)
}

func unsupportedPKCS8(der []byte) string {
	var p struct {
		Version int
		Algo    pkix.AlgorithmIdentifier
		Key     []byte
	}
	if _, err := asn1.Unmarshal(der, &p); err != nil {
		return ""
	}
	switch {
	case p.Algo.Algorithm.Equal(oidX448):
		return "X448"
	case len(p.Algo.Algorithm) == len(oidMLKEM)+1 && p.Algo.Algorithm[:len(oidMLKEM)].Equal(oidMLKEM):
		return "ML-KEM"
	}
	return ""
}

// compressedPoint is the SEC 1 compressed form of an EC public key; X25519
// keys are returned as they are.
func compressedPoint(pub *ecdh.PublicKey) []byte {
	b := pub.Bytes()
	if pub.Curve() == ecdh.X25519() {
		return b
	}
	n := (len(b) - 1) / 2
	out := make([]byte, 1+n)
	out[0] = 2 | b[len(b)-1]&1
	copy(out[1:], b[1:1+n])
	return out
}

// peerKey reads an ephemeral key from a header: compressed or uncompressed
// for the NIST curves, raw for X25519.
func peerKey(curve ecdh.Curve, b []byte) (*ecdh.PublicKey, error) {
	if c, ok := ellipticOf[curve]; ok && len(b) > 0 && (b[0] == 2 || b[0] == 3) {
		x, y := elliptic.UnmarshalCompressed(c, b)
		if x == nil {
			return nil, errors.New("refstream/key: ephemeral key not on the curve")
		}
		n := (c.Params().BitSize + 7) / 8
		u := make([]byte, 1+2*n)
		u[0] = 4
		x.FillBytes(u[1 : 1+n])
		y.FillBytes(u[1+n:])
		b = u
	}
	return curve.NewPublicKey(b)
}

// keyID is the SHA-256 of the DER SubjectPublicKeyInfo, the EC point compressed.
func keyID(pub *ecdh.PublicKey) ([]byte, error) {
	var der []byte
	if oid, ok := curveOIDs[pub.Curve()]; ok {
		params, err := asn1.Marshal(oid)
		if err != nil {
			return nil, err
		}
		pt := compressedPoint(pub)
		der, err = asn1.Marshal(struct {
			Algo pkix.AlgorithmIdentifier
			Key  asn1.BitString
		}{
			Algo: pkix.AlgorithmIdentifier{Algorithm: oidECPublicKey, Parameters: asn1.RawValue{FullBytes: params}},
			Key:  asn1.BitString{Bytes: pt, BitLength: 8 * len(pt)},
		})
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		if der, err = x509.MarshalPKIXPublicKey(pub); err != nil {
			return nil, err
		}
	}
	h := sha256.Sum256(der)
	return h[:], nil
}

// keyIDv1 is the SHA-256 of the compressed point in upper-case hex.
func keyIDv1(pub *ecdh.PublicKey) []byte {
	h := sha256.Sum256([]byte(strings.ToUpper(hex.EncodeToString(compressedPoint(pub)))))
	return h[:]
}
