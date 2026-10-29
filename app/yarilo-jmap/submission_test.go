package main

import (
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// JMAP sends through the relay SMTP Submission uses, with its limits; with no
// relay there is no submission to offer.
func TestSubmissionTakesTheSubmissionSection(t *testing.T) {
	cfg := &config.Config{}
	if submission(cfg) != nil {
		t.Fatal("submission offered with no relay host")
	}
	cfg.Protocol.Submission.Relay.Host = "relay.test"
	cfg.Protocol.Submission.MaxMsgSize = 1000
	cfg.Protocol.Submission.MaxRecipients = 5
	cfg.Protocol.Submission.AddReceivedHeader = true
	cfg.Protocol.JMAP.SubmissionTTLSeconds = 60
	got := submission(cfg)
	if got == nil || got.Sender == nil || got.MaxSize != 1000 || got.MaxRecipients != 5 || !got.AddReceived || got.TTL != time.Minute {
		t.Errorf("Submission = %+v, want the submission section's relay and limits", got)
	}
}
