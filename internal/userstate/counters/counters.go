// Package counters names the JMAP data types a push stream reports a change of.
package counters

// Type is a JMAP data type name.
type Type string

const (
	Email            Type = "Email"
	Mailbox          Type = "Mailbox"
	Thread           Type = "Thread"
	Quota            Type = "Quota"
	Identity         Type = "Identity"
	EmailSubmission  Type = "EmailSubmission"
	SieveScript      Type = "SieveScript"
	VacationResponse Type = "VacationResponse"
)

// Mail is what a written message moves: the message, the counts of its
// mailbox, its thread and the account's usage.
var Mail = []Type{Email, Mailbox, Thread, Quota}
