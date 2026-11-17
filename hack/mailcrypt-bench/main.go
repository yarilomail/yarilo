// mailcrypt-bench measures message encryption on one core (MAILCRYPT step 1b):
// age with X25519 and with the hybrid key, the reference stream's reader, and
// a 4 KiB read from the middle of a large message against reading all of it.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/agestream"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

var minTime = flag.Duration("t", 2*time.Second, "minimum time per case")

func main() {
	flag.Parse()
	runtime.GOMAXPROCS(1)
	runtime.LockOSThread()

	x, _ := age.GenerateX25519Identity()
	h, _ := age.GenerateHybridIdentity()
	keys := []struct {
		name string
		id   age.Identity
		rcpt age.Recipient
	}{{"x25519", x, x.Recipient()}, {"hybrid", h, h.Recipient()}}
	sizes := []int{4 << 10, 64 << 10, 10 << 20}

	fmt.Printf("# GOMAXPROCS=1 %s/%s %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Println("case\tsize\tmsgs/s\tMB/s")
	for _, k := range keys {
		for _, n := range sizes {
			plain := random(n)
			sealed := seal(k.rcpt, plain)
			report("age-"+k.name+"-encrypt", n, run(func() { seal(k.rcpt, plain) }))
			report("age-"+k.name+"-decrypt", n, run(func() {
				r, err := agestream.NewReader(bytes.NewReader(sealed), k.id)
				must(err)
				_, err = io.Copy(io.Discard, r)
				must(err)
			}))
		}
	}

	refKey, _ := ecdh.P256().GenerateKey(rand.Reader)
	for _, n := range sizes {
		stream := refEncode(refKey.PublicKey(), random(n))
		report("reference-p256-decrypt", n, run(func() {
			r, err := refstream.NewReader(bytes.NewReader(stream), refKey)
			must(err)
			_, err = io.Copy(io.Discard, r)
			must(err)
		}))
	}

	big := 10 << 20
	for _, k := range keys {
		sealed := seal(k.rcpt, random(big))
		whole := run(func() {
			r, err := agestream.NewReader(bytes.NewReader(sealed), k.id)
			must(err)
			_, err = io.Copy(io.Discard, r)
			must(err)
		})
		part := run(func() {
			ra, _, err := agestream.NewReaderAt(bytes.NewReader(sealed), int64(len(sealed)), k.id)
			must(err)
			_, err = ra.ReadAt(make([]byte, 4<<10), int64(big/2))
			must(err)
		})
		fmt.Printf("partial-%s\t10MiB\twhole %.2f ms\t4KiB at middle %.3f ms\n", k.name, ms(whole), ms(part))
	}
}

func run(f func()) time.Duration {
	f()
	start, n := time.Now(), 0
	for time.Since(start) < *minTime || n < 3 {
		f()
		n++
	}
	return time.Since(start) / time.Duration(n)
}

func report(name string, size int, per time.Duration) {
	fmt.Printf("%s\t%d\t%.0f\t%.1f\n", name, size, 1/per.Seconds(), float64(size)/per.Seconds()/1e6)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func seal(r age.Recipient, plain []byte) []byte {
	var b bytes.Buffer
	w, err := agestream.NewWriter(&b, r)
	must(err)
	_, err = w.Write(plain)
	must(err)
	must(w.Close())
	return b.Bytes()
}

func random(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// refEncode writes a version 2 reference stream on P-256: one key slot, the
// key data wrapped in AES-256-CBC, the body AES-256-GCM, as written by default.
func refEncode(pub *ecdh.PublicKey, plain []byte) []byte {
	material := random(32 + 12 + 16)
	eph, err := pub.Curve().GenerateKey(rand.Reader)
	must(err)
	secret, err := eph.ECDH(pub)
	must(err)
	ep := compressed(eph.PublicKey())
	tk, err := pbkdf2.Key(sha256.New, string(secret), ep, 2048, 48)
	must(err)
	wb, err := aes.NewCipher(tk[:32])
	must(err)
	padded := append(append([]byte(nil), material...), bytes.Repeat([]byte{4}, 4)...)
	wrapped := make([]byte, len(padded))
	cipher.NewCBCEncrypter(wb, tk[32:48]).CryptBlocks(wrapped, padded)

	var kd bytes.Buffer
	kd.Write([]byte{1, 2})
	kd.Write(keyID(pub))
	put32(&kd, ep)
	put32(&kd, wrapped)
	put32(&kd, stretch(material))

	calg, _ := asn1.Marshal(asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 46})
	malg, _ := asn1.Marshal(asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1})
	block, err := aes.NewCipher(material[:32])
	must(err)
	g, err := cipher.NewGCM(block)
	must(err)
	body := g.Seal(nil, material[32:44], plain, material[44:])

	var out bytes.Buffer
	out.Write(refstream.Magic)
	out.WriteByte(2)
	_ = binary.Write(&out, binary.BigEndian, uint32(2))
	_ = binary.Write(&out, binary.BigEndian, uint32(len(refstream.Magic)+9+len(calg)+len(malg)+8+kd.Len()))
	out.Write(calg)
	out.Write(malg)
	_ = binary.Write(&out, binary.BigEndian, uint32(2048))
	_ = binary.Write(&out, binary.BigEndian, uint32(kd.Len()))
	out.Write(kd.Bytes())
	out.Write(body)
	return out.Bytes()
}

func compressed(pub *ecdh.PublicKey) []byte {
	u := pub.Bytes()
	return append([]byte{2 | u[len(u)-1]&1}, u[1:33]...)
}

func keyID(pub *ecdh.PublicKey) []byte {
	params, _ := asn1.Marshal(asn1.ObjectIdentifier{1, 2, 840, 10045, 3, 1, 7})
	pt := compressed(pub)
	der, err := asn1.Marshal(struct {
		Algo pkix.AlgorithmIdentifier
		Key  asn1.BitString
	}{
		pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}, Parameters: asn1.RawValue{FullBytes: params}},
		asn1.BitString{Bytes: pt, BitLength: 8 * len(pt)},
	})
	must(err)
	h := sha256.Sum256(der)
	return h[:]
}

func stretch(data []byte) []byte {
	sum := sha256.Sum256(data)
	var i32 [4]byte
	for i := uint32(1); i < 2049; i++ {
		binary.BigEndian.PutUint32(i32[:], i)
		sum = sha256.Sum256(append(sum[:], i32[:]...))
	}
	return sum[:]
}

func put32(b *bytes.Buffer, v []byte) {
	_ = binary.Write(b, binary.BigEndian, uint32(len(v)))
	b.Write(v)
}
