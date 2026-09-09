package lmtp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// ErrRateLimited is returned by checkRecipientRate when the
// (sender IP, recipient mailbox) pair has consumed its burst
// within the current window. Callers surface it to the SMTP
// client as `421 4.7.0`.
var ErrRateLimited = errors.New("lmtp/rate: recipient rate limit exceeded")

// checkRecipientRate enforces the per-(IP, mailbox) bucket at RCPT TO. A nil
// locker or a counter error allows the delivery: this must not stop mail.
func checkRecipientRate(ctx context.Context, locker locks.Locker, ip, mailbox string, burst, windowSeconds int) error {
	if locker == nil || burst <= 0 || windowSeconds <= 0 {
		return nil
	}
	bucket := time.Now().UTC().Unix() / int64(windowSeconds)
	key := fmt.Sprintf("lmtp:rate:%s:%s:%d", ip, mailbox, bucket)
	count, err := locker.IncrementCounter(ctx, key, 1)
	if err != nil {
		return fmt.Errorf("lmtp/rate: counter inc: %w", err)
	}
	if count > int64(burst) {
		return ErrRateLimited
	}
	return nil
}
