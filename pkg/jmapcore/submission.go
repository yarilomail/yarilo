package jmapcore

import "time"

// CapSubmission is the Identity and EmailSubmission capability (RFC 8621 §1.3.2).
const CapSubmission = "urn:ietf:params:jmap:submission"

// SubmissionCapability is its per-account object.
type SubmissionCapability struct {
	MaxDelayedSend       int                 `json:"maxDelayedSend"`
	SubmissionExtensions map[string][]string `json:"submissionExtensions"`
}

// Identity is the object of RFC 8621 §6.
type Identity struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Email         string         `json:"email"`
	ReplyTo       []EmailAddress `json:"replyTo"`
	Bcc           []EmailAddress `json:"bcc"`
	TextSignature string         `json:"textSignature"`
	HTMLSignature string         `json:"htmlSignature"`
	MayDelete     bool           `json:"mayDelete"`
}

// SubmissionAddress is an envelope address with its SMTP parameters.
type SubmissionAddress struct {
	Email      string             `json:"email"`
	Parameters map[string]*string `json:"parameters"`
}

// Envelope is an EmailSubmission's SMTP envelope.
type Envelope struct {
	MailFrom SubmissionAddress   `json:"mailFrom"`
	RcptTo   []SubmissionAddress `json:"rcptTo"`
}

// EmailSubmission is the object of RFC 8621 §7.
type EmailSubmission struct {
	ID             string    `json:"id"`
	IdentityID     string    `json:"identityId"`
	EmailID        string    `json:"emailId"`
	ThreadID       string    `json:"threadId"`
	Envelope       *Envelope `json:"envelope"`
	SendAt         time.Time `json:"sendAt"`
	UndoStatus     string    `json:"undoStatus"`
	DeliveryStatus any       `json:"deliveryStatus"`
	DsnBlobIDs     []string  `json:"dsnBlobIds"`
	MdnBlobIDs     []string  `json:"mdnBlobIds"`
}

// SetError types of RFC 8621 §7.5.
const (
	SetErrInvalidEmail      = "invalidEmail"
	SetErrTooManyRecipients = "tooManyRecipients"
	SetErrNoRecipients      = "noRecipients"
	SetErrInvalidRecipients = "invalidRecipients"
	SetErrForbiddenMailFrom = "forbiddenMailFrom"
	SetErrForbiddenFrom     = "forbiddenFrom"
	SetErrForbiddenToSend   = "forbiddenToSend"
	SetErrCannotUnsend      = "cannotUnsend"
)

// FollowedBy is a method result that carries the responses of the calls it
// made implicitly, sent right after its own (RFC 8621 §7.5).
type FollowedBy struct {
	Result any
	Then   []Implicit
}

// Implicit is one implicit call's response.
type Implicit struct {
	Name   string
	Result any
}
