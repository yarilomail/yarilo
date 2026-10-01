package mailbox

import "strings"

// Auto modes a configured mailbox can take: nothing, created, or created and
// subscribed in the same step.
const (
	AutoNo        = "no"
	AutoCreate    = "create"
	AutoSubscribe = "subscribe"
)

// AutoMailbox is one entry of a namespace's mailboxes block.
type AutoMailbox struct {
	Auto       string
	SpecialUse string
}

// NormalizeAuto folds a configured auto mode; ok is false for an unknown one.
// Empty is no.
func NormalizeAuto(mode string) (string, bool) {
	switch m := strings.ToLower(strings.TrimSpace(mode)); m {
	case "", AutoNo:
		return AutoNo, true
	case AutoCreate, AutoSubscribe:
		return m, true
	default:
		return "", false
	}
}

// specialUseAttrs are the RFC 6154 attributes and RFC 8457's \Important.
var specialUseAttrs = map[string]bool{
	`\All`: true, `\Archive`: true, `\Drafts`: true, `\Flagged`: true,
	`\Junk`: true, `\Sent`: true, `\Trash`: true, `\Important`: true,
}

// IsSpecialUseAttr reports a special-use attribute a client knows.
func IsSpecialUseAttr(attr string) bool { return specialUseAttrs[attr] }
