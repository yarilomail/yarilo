package mailcrypt

import (
	"bytes"
	"crypto/ecdh"
	"io"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetrics(t *testing.T) {
	enc, _ := ageLayer(t)
	l := enc.ForDriver("metrics-test")
	other, _ := ageLayer(t)
	stranger := other.ForDriver("metrics-test")

	sealedBefore := testutil.ToFloat64(metricSealed.WithLabelValues("metrics-test"))
	body := sealed(t, l, bytes.Repeat([]byte("x"), 70000))
	if d := testutil.ToFloat64(metricSealed.WithLabelValues("metrics-test")) - sealedBefore; d != 1 {
		t.Fatalf("sealed +%v", d)
	}
	failed := func(op, reason string) float64 {
		return testutil.ToFloat64(metricFailed.WithLabelValues("metrics-test", op, reason))
	}
	cases := []struct {
		name, op, reason string
		run              func()
	}{
		{"planted", "seal", "planted", func() { _ = l.Seal(io.Discard, bytes.NewReader(body)) }},
		{"wrong key", "open", "no-key", func() { _, _ = stranger.Open(newBody(body), nil) }},
		{"corrupt chunk", "read", "integrity", func() {
			bad := append([]byte(nil), body...)
			bad[400] ^= 1 // the first chunk: the last is checked at Open, to learn the size
			if o, err := l.Open(newBody(bad), nil); err == nil {
				_, _ = io.ReadAll(o)
			}
		}},
	}
	for _, c := range cases {
		before := failed(c.op, c.reason)
		c.run()
		if d := failed(c.op, c.reason) - before; d != 1 {
			t.Errorf("%s: failed{%s,%s} +%v", c.name, c.op, c.reason, d)
		}
	}
	openedBefore := testutil.ToFloat64(metricOpened.WithLabelValues("metrics-test", "age"))
	open(t, l, body)
	if d := testutil.ToFloat64(metricOpened.WithLabelValues("metrics-test", "age")) - openedBefore; d != 1 {
		t.Errorf("opened{age} +%v", d)
	}

	stream, key, _ := referenceVector(t)
	cache, err := NewCache(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	ref := New(Options{ReferenceKeys: []*ecdh.PrivateKey{key}, Cache: cache})
	bypassBefore := testutil.ToFloat64(metricCacheBypass)
	open(t, ref, stream)
	if d := testutil.ToFloat64(metricCacheBypass) - bypassBefore; d != 1 {
		t.Errorf("cache bypass +%v", d)
	}
}
