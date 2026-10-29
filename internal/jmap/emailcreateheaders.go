package jmap

import (
	"encoding/json"
	"mime"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-message/textproto"

	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
)

// headerField is one field a create writes, with every value in the order the
// message carries them.
type headerField struct {
	name   string
	values []string
}

// emailFieldProps are the Email properties that write a header field, by the
// field's lower-case name.
var emailFieldProps = map[string]string{
	"from": "from", "to": "to", "cc": "cc", "bcc": "bcc", "replyTo": "reply-to", "sender": "sender",
	"subject": "subject", "sentAt": "date", "messageId": "message-id", "inReplyTo": "in-reply-to",
	"references": "references",
}

// headerLevel is where a header:* property sits: the Email or a body part.
type headerLevel int

const (
	emailLevel headerLevel = iota
	partLevel
)

// headerProps reads the header:* properties of one level of a create (RFC 8621
// §4.6); taken maps the fields its other properties write to those properties.
func headerProps(props map[string]json.RawMessage, taken map[string]string, level headerLevel) ([]headerField, []string) {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var bad []string
	byField := map[string][]string{}
	parsed := map[string]jmapcore.HeaderProperty{}
	for _, k := range keys {
		hp, ok := jmapcore.ParseHeaderProperty(k)
		if !ok || !fieldNameValid(hp.Name) {
			bad = append(bad, k)
			continue
		}
		n := strings.ToLower(hp.Name)
		switch {
		case level == emailLevel && strings.HasPrefix(n, "content-"),
			level == partLevel && n == "content-transfer-encoding",
			!jmapcore.HeaderFormAllowed(hp.Name, hp.Form):
			bad = append(bad, k)
			continue
		}
		if p, ok := taken[n]; ok {
			bad = append(bad, k, p)
			continue
		}
		parsed[k] = hp
		byField[n] = append(byField[n], k)
	}
	var out []headerField
	for _, k := range keys {
		hp, ok := parsed[k]
		if !ok {
			continue
		}
		if same := byField[strings.ToLower(hp.Name)]; len(same) > 1 {
			bad = append(bad, k)
			continue
		}
		values, ok := headerValues(hp, props[k])
		if !ok {
			bad = append(bad, k)
			continue
		}
		if values != nil {
			out = append(out, headerField{name: hp.Name, values: values})
		}
	}
	return out, dedupSorted(bad)
}

// fieldNameValid reports whether a name is an RFC 5322 field name: printable
// US-ASCII without the colon.
func fieldNameValid(name string) bool {
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < '!' || c > '~' || c == ':' {
			return false
		}
	}
	return name != ""
}

// headerValues renders a property's value as the field values it writes; nil
// with ok means the property is null and writes nothing.
func headerValues(hp jmapcore.HeaderProperty, raw json.RawMessage) ([]string, bool) {
	if string(raw) == "null" {
		return nil, true
	}
	if !hp.All {
		v, ok := headerValue(hp.Form, raw)
		if !ok {
			return nil, false
		}
		return []string{v}, true
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		v, ok := headerValue(hp.Form, item)
		if !ok {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// headerValue renders one value of a form; a line break left in it would be a
// second field written under the first one's name, so it is refused.
func headerValue(form jmapcore.HeaderForm, raw json.RawMessage) (string, bool) {
	var v string
	switch form {
	case jmapcore.FormRaw:
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		u, ok := unfoldRaw(s)
		if !ok {
			return "", false
		}
		// The raw form starts with the space after the colon; the writer adds it.
		v = strings.TrimPrefix(u, " ")
	case jmapcore.FormText:
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		v = mime.QEncoding.Encode("utf-8", s)
	case jmapcore.FormAddresses:
		var list []jmapcore.EmailAddress
		if json.Unmarshal(raw, &list) != nil {
			return "", false
		}
		v = addressList(list)
	case jmapcore.FormGroupedAddresses:
		var groups []jmapcore.EmailAddressGroup
		if json.Unmarshal(raw, &groups) != nil {
			return "", false
		}
		parts := make([]string, 0, len(groups))
		for _, g := range groups {
			if g.Name == nil {
				if len(g.Addresses) > 0 {
					parts = append(parts, addressList(g.Addresses))
				}
				continue
			}
			parts = append(parts, phrase(*g.Name)+": "+addressList(g.Addresses)+";")
		}
		v = strings.Join(parts, ", ")
	case jmapcore.FormMessageIDs:
		var ids []string
		if json.Unmarshal(raw, &ids) != nil {
			return "", false
		}
		for i, id := range ids {
			ids[i] = "<" + strings.Trim(id, "<>") + ">"
		}
		v = strings.Join(ids, " ")
	case jmapcore.FormDate:
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return "", false
		}
		v = t.Format("Mon, 02 Jan 2006 15:04:05 -0700")
	case jmapcore.FormURLs:
		var urls []string
		if json.Unmarshal(raw, &urls) != nil {
			return "", false
		}
		for i, u := range urls {
			urls[i] = "<" + u + ">"
		}
		v = strings.Join(urls, ", ")
	default:
		return "", false
	}
	if strings.ContainsAny(v, "\r\n\x00") {
		return "", false
	}
	return v, true
}

// unfoldRaw removes the folding of a raw value (RFC 5322 §2.2.3). A line
// break that is not followed by whitespace is not folding, and is refused.
func unfoldRaw(s string) (string, bool) {
	if !strings.ContainsAny(s, "\r\n") {
		return s, true
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\r':
			if i+1 >= len(s) || s[i+1] != '\n' {
				return "", false
			}
		case '\n':
			if i+1 >= len(s) || (s[i+1] != ' ' && s[i+1] != '\t') {
				return "", false
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), true
}

func addressList(list []jmapcore.EmailAddress) string {
	addrs := mailAddresses(list)
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	return strings.Join(out, ", ")
}

// phrase writes a group's display name as words, quoted, or encoded
// (RFC 5322 §3.2.5, RFC 2047).
func phrase(s string) string {
	plain := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			return mime.QEncoding.Encode("utf-8", s)
		}
		if !(c == ' ' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
			strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0) {
			plain = false
		}
	}
	if plain && s != "" {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// putFields writes fields over h, each replacing any field of its name.
func putFields(h *textproto.Header, fields []headerField) {
	for _, f := range fields {
		h.Del(f.name)
		// Add puts a field on top, so the values go in last to first.
		for i := len(f.values) - 1; i >= 0; i-- {
			h.Add(f.name, f.values[i])
		}
	}
}

// partFieldProps maps the fields a body part's own properties write to the
// first property that writes each.
func partFieldProps(p *createPart) map[string]string {
	taken := map[string]string{}
	set := func(field, prop string, given bool) {
		if _, ok := taken[field]; given && !ok {
			taken[field] = prop
		}
	}
	set("content-type", "type", p.typeGiven)
	set("content-type", "charset", p.Charset != nil)
	set("content-type", "name", p.Name != nil)
	set("content-disposition", "disposition", p.Disposition != nil)
	set("content-disposition", "name", p.Name != nil)
	set("content-id", "cid", p.CID != nil)
	set("content-language", "language", len(p.Language) > 0)
	set("content-location", "location", p.Location != nil)
	return taken
}

// takeField removes a field from fields and returns its last value.
func takeField(fields *[]headerField, name string) (string, bool) {
	for i, f := range *fields {
		if strings.EqualFold(f.name, name) {
			*fields = append((*fields)[:i:i], (*fields)[i+1:]...)
			if len(f.values) == 0 {
				return "", false
			}
			return f.values[len(f.values)-1], true
		}
	}
	return "", false
}

func dedupSorted(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	sort.Strings(list)
	out := list[:1]
	for _, s := range list[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
