package jmapcore

import "strings"

// HeaderProperty is a parsed "header:{name}[:as{Form}][:all]" property
// (RFC 8621 §4.1.3).
//
// These matter beyond conformance: they are the only way a client reaches a
// header the data type does not model. List-Unsubscribe, X-Spam-*, DKIM
// results - all present in the message and, without this, unreachable over
// JMAP.
type HeaderProperty struct {
	// Property is the name as the client wrote it, which is the key the answer
	// must come back under.
	Property string
	// Name is the header field name, matched case-insensitively.
	Name string
	// Form is the requested form; Raw when the client named none.
	Form HeaderForm
	// All requests every occurrence rather than the last one.
	All bool
}

// HeaderForm is one of the forms of §4.1.3.
type HeaderForm string

// The header forms, by their property suffix.
const (
	FormRaw              HeaderForm = "asRaw"
	FormText             HeaderForm = "asText"
	FormAddresses        HeaderForm = "asAddresses"
	FormGroupedAddresses HeaderForm = "asGroupedAddresses"
	FormMessageIDs       HeaderForm = "asMessageIds"
	FormDate             HeaderForm = "asDate"
	FormURLs             HeaderForm = "asURLs"
)

var headerForms = map[string]HeaderForm{
	string(FormRaw):              FormRaw,
	string(FormText):             FormText,
	string(FormAddresses):        FormAddresses,
	string(FormGroupedAddresses): FormGroupedAddresses,
	string(FormMessageIDs):       FormMessageIDs,
	string(FormDate):             FormDate,
	string(FormURLs):             FormURLs,
}

// headerPrefix is what marks a property as a header field request.
const headerPrefix = "header:"

// IsHeaderProperty reports whether a property names a header field.
func IsHeaderProperty(p string) bool {
	return strings.HasPrefix(p, headerPrefix)
}

// ParseHeaderProperty splits a header property into its parts.
//
// ok=false means the property is malformed - an empty field name, an unknown
// form, or suffixes in the wrong order. It is deliberately not "assume Raw and
// carry on": a client asking for asURLs and silently receiving a raw string
// gets a type it did not ask for, and would have no way to notice.
func ParseHeaderProperty(p string) (HeaderProperty, bool) {
	if !IsHeaderProperty(p) {
		return HeaderProperty{}, false
	}
	rest := p[len(headerPrefix):]
	out := HeaderProperty{Property: p, Form: FormRaw}

	// ":all" is the last suffix when present (§4.1.3), so it comes off first.
	if trimmed, found := strings.CutSuffix(rest, ":all"); found {
		out.All = true
		rest = trimmed
	}
	// A form suffix, if any, follows the first colon. RFC 5322 field names are
	// printable ASCII excluding the colon, so the first colon is the only place
	// a suffix can begin and everything after it must be the form - which is
	// why "header:To:asText:asRaw" is refused rather than read as a field named
	// "To:asText".
	if name, form, found := strings.Cut(rest, ":"); found {
		f, known := headerForms[form]
		if !known {
			return HeaderProperty{}, false
		}
		out.Form, rest = f, name
	}
	if rest == "" {
		return HeaderProperty{}, false
	}
	out.Name = rest
	return out, true
}

// definedFields are the header fields RFC 5322 and RFC 2369 define. Any other
// field may be read or set in every form (RFC 8621 §4.1.2).
var definedFields = map[string]bool{
	"date": true, "from": true, "sender": true, "reply-to": true, "to": true, "cc": true, "bcc": true,
	"message-id": true, "in-reply-to": true, "references": true, "subject": true, "comments": true, "keywords": true,
	"resent-date": true, "resent-from": true, "resent-sender": true, "resent-to": true, "resent-cc": true,
	"resent-bcc": true, "resent-message-id": true, "return-path": true, "received": true,
	"list-help": true, "list-unsubscribe": true, "list-subscribe": true, "list-post": true, "list-owner": true,
	"list-archive": true,
}

var addressFields = map[string]bool{
	"from": true, "sender": true, "reply-to": true, "to": true, "cc": true, "bcc": true,
	"resent-from": true, "resent-sender": true, "resent-reply-to": true, "resent-to": true, "resent-cc": true,
	"resent-bcc": true,
}

// formFields are the defined fields each parsed form is allowed for
// (RFC 8621 §4.1.2.2 to §4.1.2.7).
var formFields = map[HeaderForm]map[string]bool{
	FormText:             {"subject": true, "comments": true, "keywords": true, "list-id": true},
	FormAddresses:        addressFields,
	FormGroupedAddresses: addressFields,
	FormMessageIDs:       {"message-id": true, "in-reply-to": true, "references": true, "resent-message-id": true},
	FormDate:             {"date": true, "resent-date": true},
	FormURLs: {"list-help": true, "list-unsubscribe": true, "list-subscribe": true, "list-post": true,
		"list-owner": true, "list-archive": true},
}

// HeaderFormAllowed reports whether a field may be given in a form.
func HeaderFormAllowed(name string, form HeaderForm) bool {
	if form == FormRaw {
		return true
	}
	n := strings.ToLower(name)
	return formFields[form][n] || !definedFields[n]
}
