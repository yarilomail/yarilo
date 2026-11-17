package mailcrypt

import (
	"errors"
	"io"
	"time"

	"filippo.io/age"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

var (
	metricSealed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mailbox_crypt_sealed_total",
		Help: "Message bodies encrypted on save, by driver.",
	}, []string{"driver"})
	metricOpened = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mailbox_crypt_opened_total",
		Help: "Message bodies opened, by driver and stored format: plain, age, or reference (a store taken from another server's disk).",
	}, []string{"driver", "format"})
	metricFailed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mailbox_crypt_failed_total",
		Help: "Bodies the layer refused, by driver, operation (seal, open, read, size) and reason: no-key, integrity, truncated, unsupported, planted, other.",
	}, []string{"driver", "op", "reason"})
	metricSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mailbox_crypt_seconds",
		Help:    "Time to seal a body, or to open one up to its first byte, by operation and format.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11),
	}, []string{"op", "format"})
	metricCacheBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mailbox_crypt_cache_bytes",
		Help: "Decrypted reference-format bodies held in storage.mail_temp_dir by this process.",
	})
	metricCacheBypass = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mailbox_crypt_cache_bypass_total",
		Help: "Reference-format bodies read as a stream because storage.mail_crypt.reference_cache_size was full.",
	})
)

func kindName(k Kind) string {
	switch k {
	case Age:
		return "age"
	case Reference:
		return "reference"
	}
	return "plain"
}

// reason names a refusal for the failed counter.
func reason(err error) string {
	var noMatch *age.NoIdentityMatchError
	switch {
	case errors.Is(err, ErrPlanted):
		return "planted"
	case errors.Is(err, ErrNoKey), errors.As(err, &noMatch), errors.Is(err, refstream.ErrNoKey):
		return "no-key"
	case errors.Is(err, refstream.ErrIntegrity):
		return "integrity"
	case errors.Is(err, refstream.ErrTruncated), errors.Is(err, io.ErrUnexpectedEOF):
		return "truncated"
	case errors.Is(err, refstream.ErrUnsupported):
		return "unsupported"
	}
	return "other"
}

// countFailure records err under op and returns it.
func (l *Layer) countFailure(op string, err error) error {
	if err != nil {
		r := reason(err)
		// age's chunk and header errors are untyped: past the key, a body
		// that does not open or read is one that does not verify.
		if r == "other" && (op == "open" || op == "read") {
			r = "integrity"
		}
		metricFailed.WithLabelValues(l.driverName(), op, r).Inc()
	}
	return err
}

// FailedSize counts a plaintext size that could not be told; the driver
// lists the body as unknown.
func (l *Layer) FailedSize(err error) { _ = l.countFailure("size", err) }

func observe(op string, k Kind, start time.Time) {
	metricSeconds.WithLabelValues(op, kindName(k)).Observe(time.Since(start).Seconds())
}

// countingOpened counts the first read failure of an opened body: age and a
// reference body check integrity as they are read, after Open succeeded.
type countingOpened struct {
	Opened
	l      *Layer
	failed bool
}

func (c *countingOpened) note(err error) {
	if err != nil && !errors.Is(err, io.EOF) && !c.failed {
		c.failed = true
		// Past Open the stream is authenticated as it is read: a failure
		// that is not a cut file is a chunk or a tag that does not verify.
		_ = c.l.countFailure("read", err)
	}
}

func (c *countingOpened) Read(p []byte) (int, error) {
	n, err := c.Opened.Read(p)
	c.note(err)
	return n, err
}

func (c *countingOpened) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.Opened.ReadAt(p, off)
	c.note(err)
	return n, err
}
