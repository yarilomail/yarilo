package mailcrypt

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/agestream"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		prefix []byte
		want   Kind
	}{
		{"age", append(append([]byte(nil), agestream.Magic...), "-> X25519"...), Age},
		{"reference", append(append([]byte(nil), refstream.Magic...), 2, 0, 0), Reference},
		{"plain", []byte("Received: from mx\r\n"), Plain},
		{"empty", nil, Plain},
		{"age cut short", agestream.Magic[:10], Plain},
	}
	for _, c := range cases {
		if got := Classify(c.prefix); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if PrefixLen < len(agestream.Magic) || PrefixLen < len(refstream.Magic) {
		t.Fatalf("PrefixLen %d shorter than a signature", PrefixLen)
	}
}
