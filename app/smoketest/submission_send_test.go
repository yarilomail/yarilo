package main

import (
	"net"
	"testing"
	"time"
)

// smtpReply returns the last line of a multi-line reply and leaves nothing of
// it for the next read.
func TestSMTPReplyReadsTheWholeReply(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go b.Write([]byte("250-Hello\r\n250-PIPELINING\r\n250 SIZE 10\r\n250 2.0.0 OK\r\n")) //nolint:errcheck
	a.SetDeadline(time.Now().Add(2 * time.Second))                                       //nolint:errcheck
	first, err := smtpReply(a)
	if err != nil || first != "250 SIZE 10" {
		t.Fatalf("first reply = %q (%v), want the EHLO block's last line", first, err)
	}
	if next, err := smtpReply(a); err != nil || next != "250 2.0.0 OK" {
		t.Errorf("next reply = %q (%v), want the next command's", next, err)
	}
}
