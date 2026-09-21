// Package mailboxmetrics holds the timings shared by the mailbox drivers.
//
// One metric name per question, with the driver as a label, because the
// question these answer is comparative: what a save costs on mdbox is only
// meaningful beside what it costs on maildir. Two names would make that a
// join, and a join is where a comparison quietly stops being made.
package mailboxmetrics

import (
	"errors"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

var (
	saveSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mailbox_save_seconds",
		Help:    "Time to store one message, whole, by driver. Failures are included: a save that fails is a cost the storage paid, and it reports fewer parts than a successful one -- so a remainder computed over a window with failures in it is larger than the unnamed cost.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11), // 10us .. ~10s
	}, []string{"driver"})

	savePartSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mailbox_save_part_seconds",
		Help:    "Time in one named part of storing a message. The parts sum to no more than the whole; what is left over is a cost nobody has named yet.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11),
	}, []string{"driver", "part"})
)

// ObserveSave records one whole save.
func ObserveSave(driver string, d time.Duration) {
	saveSeconds.WithLabelValues(driver).Observe(d.Seconds())
}

// ObserveSavePart records one named step of a save. A driver that does not
// have a given step simply never reports it.
func ObserveSavePart(driver, part string, d time.Duration) {
	savePartSeconds.WithLabelValues(driver, part).Observe(d.Seconds())
}

// A volume that is full refuses the body long before the journal, so the class
// has to be named at every write a delivery makes (#1831).
var writeFailed = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "mailbox_write_failed_total",
	Help: "Writes a driver's storage refused, by driver and what refused them: no-space is a full volume or an exhausted quota, other is everything else.",
}, []string{"driver", "reason"})

// ClassifyWrite names what the volume refused and counts it. A full volume is
// a resource condition: the same write works once there is room.
func ClassifyWrite(driver, folder string, err error) error {
	if err == nil {
		return nil
	}
	reason := "other"
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		reason = "no-space"
		err = &mailbox.NoSpaceError{Folder: folder, Err: err}
	}
	writeFailed.WithLabelValues(driver, reason).Inc()
	return err
}

// messageOpened counts message bodies opened from a record. It is how a cache
// claim is checked: a listing answered from the cache opens nothing (#1714).
var messageOpened = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "mailbox_message_opened_total",
	Help: "Message bodies opened from their index record, by driver. A listing served from the index cache adds none.",
}, []string{"driver"})

// ObserveOpen records one message body opened.
func ObserveOpen(driver string) {
	messageOpened.WithLabelValues(driver).Inc()
}

// MessageOpens is what the counter holds for a driver, for the rows that
// assert a listing opened nothing.
func MessageOpens(driver string) float64 {
	m := &dto.Metric{}
	if err := messageOpened.WithLabelValues(driver).Write(m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}
