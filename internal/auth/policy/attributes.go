package policy

import (
	"fmt"
	"strings"

	"github.com/yarilomail/yarilo/pkg/sqlvars"
)

// DefaultRequestAttributes is the request body when none is configured: the
// keys a wforce policy written for the reference reads.
var DefaultRequestAttributes = map[string]string{
	"login":      "%{requested_username}",
	"pwhash":     "%{hashed_password}",
	"remote":     "%{remote_ip}",
	"device_id":  "%{client_id}",
	"protocol":   "%{protocol}",
	"session_id": "%{session}",
	"fail_type":  "%{fail_type}",
}

// attrVars are the variables an attribute may name.
var attrVars = map[string]func(r Request, pwhash string) string{
	"requested_username": func(r Request, _ string) string { return r.Username },
	"user":               func(r Request, _ string) string { return r.Username },
	"username":           func(r Request, _ string) string { l, _ := sqlvars.SplitUser(r.Username); return l },
	"domain":             func(r Request, _ string) string { _, d := sqlvars.SplitUser(r.Username); return d },
	"hashed_password":    func(_ Request, h string) string { return h },
	"protocol":           func(r Request, _ string) string { return sqlvars.ProtocolName(r.Service) },
	"service":            func(r Request, _ string) string { return sqlvars.ProtocolName(r.Service) },
	"remote_ip":          func(r Request, _ string) string { return r.RemoteIP },
	"rip":                func(r Request, _ string) string { return r.RemoteIP },
	"local_ip":           func(r Request, _ string) string { return r.LocalIP },
	"lip":                func(r Request, _ string) string { return r.LocalIP },
	"session":            func(r Request, _ string) string { return r.SessionID },
	"client_id":          func(r Request, _ string) string { return r.DeviceID },
	"fail_type":          func(r Request, _ string) string { return r.FailType },
}

// attribute is one body key and its value template, split into literals and
// variables at load.
type attribute struct {
	object, key string // object is empty for a top-level key
	parts       []templatePart
}

type templatePart struct {
	literal string
	expand  func(Request, string) string
}

// compileAttributes parses every template, refusing an unknown variable: sent
// as empty, it would look to the policy like a client that sent nothing.
func compileAttributes(attrs map[string]string) ([]attribute, error) {
	if attrs == nil {
		attrs = DefaultRequestAttributes
	}
	out := make([]attribute, 0, len(attrs))
	for key, tmpl := range attrs {
		parts, err := compileTemplate(tmpl)
		if err != nil {
			return nil, fmt.Errorf("policy: auth_policy_request_attributes %s: %w", key, err)
		}
		a := attribute{key: key, parts: parts}
		// A key/subkey is subkey inside the object key, as the reference nests.
		if obj, sub, ok := strings.Cut(key, "/"); ok {
			if obj == "" || sub == "" {
				return nil, fmt.Errorf("policy: auth_policy_request_attributes %q: empty object or key", key)
			}
			a.object, a.key = obj, sub
		}
		out = append(out, a)
	}
	for _, a := range out {
		if a.object != "" {
			if _, clash := attrs[a.object]; clash {
				return nil, fmt.Errorf("policy: auth_policy_request_attributes %q is both a value and an object", a.object)
			}
		}
	}
	return out, nil
}

func compileTemplate(s string) ([]templatePart, error) {
	var parts []templatePart
	var lit strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			lit.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '%' {
			lit.WriteByte('%')
			i++
			continue
		}
		if i+1 >= len(s) || s[i+1] != '{' {
			return nil, fmt.Errorf("a %% not followed by {name} at offset %d", i)
		}
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			return nil, fmt.Errorf("unterminated %%{ at offset %d", i)
		}
		name := strings.TrimSpace(s[i+2 : i+2+end])
		fn, ok := attrVars[name]
		if !ok {
			return nil, fmt.Errorf("unknown variable %%{%s}", name)
		}
		if lit.Len() > 0 {
			parts = append(parts, templatePart{literal: lit.String()})
			lit.Reset()
		}
		parts = append(parts, templatePart{expand: fn})
		i += end + 2
	}
	if lit.Len() > 0 {
		parts = append(parts, templatePart{literal: lit.String()})
	}
	return parts, nil
}

func (a attribute) value(r Request, pwhash string) string {
	var b strings.Builder
	for _, p := range a.parts {
		if p.expand != nil {
			b.WriteString(p.expand(r, pwhash))
		} else {
			b.WriteString(p.literal)
		}
	}
	return b.String()
}
