package msgcache

import (
	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/imaptext"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// StoreEnvelope seeds the cache from a struct. Test-only on purpose: a writer
// that builds the text from anything but the raw header is the second rule
// that made #2008, so production has no such path.
func (fc *Handle) StoreEnvelope(m *mailbox.MessageMeta, env *imaplib.Envelope) {
	if fc == nil || env == nil {
		return
	}
	fc.StoreEnvelopeText(m, imaptext.WriteEnvelope(env))
}
