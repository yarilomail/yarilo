package msgcache

import (
	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/imaptext"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// StoreEnvelope seeds the cache from a struct. Test-only: a writer building
// the text from anything but the raw header is what made #2008.
func (fc *Handle) StoreEnvelope(m *mailbox.MessageMeta, env *imaplib.Envelope) {
	if fc == nil || env == nil {
		return
	}
	fc.StoreEnvelopeText(m, imaptext.WriteEnvelope(env))
}
