package main

import (
	"strings"
	"testing"
)

// A delivery is refused at the final dot, and only there: every other step of
// the session already answers 250, so a transcript-wide match sees a send.
func TestARefusedDeliveryIsNotReadAsSent(t *testing.T) {
	rows := []struct {
		name  string
		reply string
	}{
		{"over quota", "452 4.2.2 Mailbox full"},
		{"sieve reject", "550 5.7.1 smoke test reject"},
		{"too large", "552 5.2.3 Requested allocation size exceeds max mail size"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			host, port := fakeDeliveryServerReply(t, true, row.reply)
			pointDeliveryAt(t, host, port, true)

			err := lmtpSend("id-1", "from@test.invalid", "to@test.invalid", "subject", "body")
			if err == nil {
				t.Fatalf("lmtpSend accepted a delivery refused with %q", row.reply)
			}
			if !strings.Contains(err.Error(), row.reply) {
				t.Errorf("lmtpSend error %v does not carry the refusal %q", err, row.reply)
			}

			err = lmtpSendRaw("from@test.invalid", "to@test.invalid", "Subject: raw\r\n\r\nbody")
			if err == nil {
				t.Fatalf("lmtpSendRaw accepted a delivery refused with %q", row.reply)
			}
			if !strings.Contains(err.Error(), row.reply) {
				t.Errorf("lmtpSendRaw error %v does not carry the refusal %q", err, row.reply)
			}
		})
	}
}

// The accepted case keeps its verdict: the assertion is on the code, not on the
// presence of a reply.
func TestAnAcceptedDeliveryStillPasses(t *testing.T) {
	host, port := fakeDeliveryServerReply(t, true, "250 2.0.0 accepted")
	pointDeliveryAt(t, host, port, true)
	if err := lmtpSend("id-2", "from@test.invalid", "to@test.invalid", "subject", "body"); err != nil {
		t.Fatalf("lmtpSend: %v", err)
	}
	if err := lmtpSendRaw("from@test.invalid", "to@test.invalid", "Subject: raw\r\n\r\nbody"); err != nil {
		t.Fatalf("lmtpSendRaw: %v", err)
	}
}

// lmtpDeliver hands the reply back instead of judging it: that is what a caller
// expecting a refusal reads the code from.
func TestTheDeliverHelperReturnsTheReplyToTheDot(t *testing.T) {
	host, port := fakeDeliveryServerReply(t, true, "550 5.7.1 rejected")
	pointDeliveryAt(t, host, port, true)
	resp, err := lmtpDeliver("id-3", "from@test.invalid", "to@test.invalid", "subject", "body")
	if err != nil {
		t.Fatalf("lmtpDeliver: %v", err)
	}
	if resp != "550 5.7.1 rejected" {
		t.Errorf("lmtpDeliver returned %q, want the reply to the dot", resp)
	}
}
