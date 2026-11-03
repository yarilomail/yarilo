package jmap

import (
	"log/slog"
	"strings"

	"github.com/emersion/go-message"

	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// headerEvaluator answers the header conditions without yarilo-fts the way
// IMAP SEARCH does without it, by reading each candidate's header; only text
// and body need the full-text index (#2452).
type headerEvaluator struct {
	indexEvaluator
	mbox mailbox.Box
}

// unsupported names the conditions that read the body.
func (headerEvaluator) unsupported(f *jmapcore.EmailFilter) []string {
	var out []string
	for _, name := range f.TextConditions() {
		if name == "text" || name == "body" {
			out = append(out, name)
		}
	}
	return out
}

func (e headerEvaluator) match(m *mailbox.MessageMeta, sf scopeFolder, scope *queryScope, f *jmapcore.EmailFilter) bool {
	if !e.indexEvaluator.match(m, sf, scope, f) {
		return false
	}
	terms, present := headerConditions(f)
	if len(terms) == 0 && present == "" {
		return true
	}
	hdr, err := e.readHeader(sf, m)
	if err != nil {
		slog.Warn("jmap: cannot read a header to match", "folder", sf.name, "uid", m.UID, "err", err)
		return false
	}
	if present != "" && !hdr.Has(present) {
		return false
	}
	for _, t := range terms {
		if !t.matches(hdr, "") {
			return false
		}
	}
	return true
}

func (e headerEvaluator) readHeader(sf scopeFolder, m *mailbox.MessageMeta) (message.Header, error) {
	rc, err := e.mbox.OpenMessage(sf.name, m)
	if err != nil {
		return message.Header{}, err
	}
	defer rc.Close() //nolint:errcheck
	entity, err := message.Read(rc)
	if err != nil && entity == nil {
		return message.Header{}, err
	}
	return entity.Header, nil
}

// headerConditions are the filter's header conditions as substring terms, and
// the field a header condition asks only to be present (RFC 8621 §4.4.1).
func headerConditions(f *jmapcore.EmailFilter) (terms []verifyTerm, present string) {
	if f == nil {
		return nil, ""
	}
	for _, h := range []struct {
		name  string
		value *string
	}{{"subject", f.Subject}, {"from", f.From}, {"to", f.To}, {"cc", f.Cc}, {"bcc", f.Bcc}} {
		if v := deref(h.value); v != "" {
			terms = append(terms, verifyTerm{headers: []string{h.name}, want: v})
		}
	}
	if len(f.Header) > 0 {
		present = strings.ToLower(f.Header[0])
		if len(f.Header) > 1 && f.Header[1] != "" {
			terms = append(terms, verifyTerm{headers: []string{present}, want: f.Header[1]})
		}
	}
	return terms, present
}
