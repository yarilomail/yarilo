package ftsservice

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// A retraction that names no message is refused, counted and logged. Once the
// index stops keeping a term per copy it cannot be carried out at all, and a
// caller that lost the identity must find out here rather than from an index
// answering with deleted mail (#1986).
func TestExpungeWithoutAMessageIsRefused(t *testing.T) {
	svc := &Service{}
	before := testutil.ToFloat64(metricExpungeNoGUID)

	err := svc.Expunge("u@test", fts.MailboxRef{Name: "INBOX", GUID: "g1", UIDValidity: 1}, 7, [16]byte{})
	if err == nil {
		t.Fatal("a retraction naming no message was accepted")
	}
	if !strings.Contains(err.Error(), "names no message") {
		t.Errorf("the refusal reads %q, which does not say why", err)
	}
	if now := testutil.ToFloat64(metricExpungeNoGUID); now != before+1 {
		t.Errorf("the refusal moved the counter by %v, want 1", now-before)
	}
}
