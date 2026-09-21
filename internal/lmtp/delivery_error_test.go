package lmtp

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/internal/storage/mailboxmetrics"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A full volume must not bounce the message: 5xx tells the sending MTA never,
// and the mail is gone before anybody frees a byte.
func TestDeliveryErrorAnswersTheResourceClass(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		verbose  bool
		wantCode int
		wantEnh  goSmtp.EnhancedCode
	}{
		{
			name:     "a volume with no room left",
			err:      fmt.Errorf("maildir: write: %w", mailboxmetrics.ClassifyWrite("maildir", "INBOX", syscall.ENOSPC)),
			wantCode: 452,
			wantEnh:  goSmtp.EnhancedCode{4, 3, 1},
		},
		{
			name:     "the journal, once the body is written",
			err:      fmt.Errorf("fileindex/mutlog: write: %w", &mailbox.NoSpaceError{Folder: "INBOX", Err: errors.New("no space left on device")}),
			wantCode: 452,
			wantEnh:  goSmtp.EnhancedCode{4, 3, 1},
		},
		{
			name:     "the same volume, verbose replies on",
			err:      fmt.Errorf("maildir: write: %w", mailboxmetrics.ClassifyWrite("maildir", "INBOX", syscall.ENOSPC)),
			verbose:  true,
			wantCode: 452,
			wantEnh:  goSmtp.EnhancedCode{4, 3, 1},
		},
		{
			name:     "any other failure keeps the answer it had",
			err:      errors.New("input/output error"),
			wantCode: 451,
			wantEnh:  goSmtp.EnhancedCode{4, 2, 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var se *goSmtp.SMTPError
			if !errors.As(deliveryError(tc.err, tc.verbose), &se) {
				t.Fatal("the answer is not an SMTP error, so the client is told nothing it can act on")
			}
			if se.Code != tc.wantCode {
				t.Errorf("code = %d, want %d", se.Code, tc.wantCode)
			}
			if se.EnhancedCode != tc.wantEnh {
				t.Errorf("enhanced code = %v, want %v", se.EnhancedCode, tc.wantEnh)
			}
		})
	}
}
