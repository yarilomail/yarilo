package jmap

import (
	"strings"

	"github.com/emersion/go-message"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// partBlobID names one leaf part of a message (#2208): P, the message's id,
// a dash, the part id. It resolves only through the caller's own mail.
func partBlobID(m *mailbox.MessageMeta, partID string) string {
	return "P" + emailID(m) + "-" + partID
}

// parsePartBlobID splits a part blob id; ok is false for anything else.
func parsePartBlobID(id string) (email, partID string, ok bool) {
	rest, found := strings.CutPrefix(id, "P")
	if !found {
		return "", "", false
	}
	email, partID, found = strings.Cut(rest, "-")
	if !found || len(email) != 32 || !validPartID(partID) {
		return "", "", false
	}
	if _, err := parseObjectID(email); err != nil {
		return "", "", false
	}
	return email, partID, true
}

// validPartID accepts dotted positive numbers, the form collectParts makes.
func validPartID(s string) bool {
	if s == "" {
		return false
	}
	for _, seg := range strings.Split(s, ".") {
		if seg == "" || seg[0] == '0' || strings.Trim(seg, "0123456789") != "" {
			return false
		}
	}
	return true
}

// partBody is the decoded body of one leaf part, walked as Email/get walks it.
func partBody(e *message.Entity, partID string) (string, bool) {
	for _, p := range collectParts(e, "") {
		if p.part.PartID != nil && *p.part.PartID == partID {
			return p.body, true
		}
	}
	return "", false
}
