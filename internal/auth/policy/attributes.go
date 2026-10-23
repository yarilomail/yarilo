package policy

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/yarilomail/yarilo/pkg/expand"
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

// VariableNames are the variables a template may name besides the providers:
// the auth request's and the three the policy adds.
var VariableNames = []string{
	"user", "protocol", "home", "local_ip", "remote_ip", "client_pid", "password", "id",
	"mechanism", "secured", "local_port", "remote_port", "cert", "login_user", "session",
	"real_local_ip", "real_remote_ip", "real_local_port", "real_remote_port", "domain_first",
	"domain_last", "master_user", "session_pid", "original_user", "auth_user", "local_name",
	"client_id", "ssl_ja3_hash", "ssl_client_cert_fp", "ssl_client_cert_pubkey_fp", "owner_user",
	"requested_username", "hashed_password", "fail_type",
}

// attribute is one body value: its path of object names and its template.
type attribute struct {
	path []string
	prog *expand.Program
}

// compileAttributes parses every template and expands it once against an
// empty request, so a template naming an unknown variable stops the service
// at start rather than sending empty values.
// processProviders read yarilo-auth's own environment, which must not leave
// the cluster in a policy body.
var processProviders = map[string]bool{"env": true, "process": true, "system": true}

func compileAttributes(attrs map[string]string) ([]attribute, error) {
	if attrs == nil {
		attrs = DefaultRequestAttributes
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	empty := make(map[string]string, len(VariableNames))
	for _, n := range VariableNames {
		empty[n] = ""
	}
	out := make([]attribute, 0, len(keys))
	leaves := map[string]bool{}
	for _, key := range keys {
		path := strings.Split(key, "/")
		for _, part := range path {
			if part == "" {
				return nil, fmt.Errorf("policy: auth_policy_request_attributes %q: empty object or key", key)
			}
		}
		leaves[key] = true
		prog, err := expand.Parse(attrs[key])
		if err != nil {
			return nil, fmt.Errorf("policy: auth_policy_request_attributes %s: %w", key, err)
		}
		for _, n := range prog.Names() {
			if prefix, _, ok := strings.Cut(n, ":"); ok && processProviders[prefix] {
				return nil, fmt.Errorf("policy: auth_policy_request_attributes %s: %%{%s} reads yarilo-auth itself, refused", key, n)
			}
		}
		if _, err := prog.Execute(templateParams(empty, nil, nil)); err != nil {
			return nil, fmt.Errorf("policy: auth_policy_request_attributes %s: %w", key, err)
		}
		out = append(out, attribute{path: path, prog: prog})
	}
	for _, key := range keys {
		for i := strings.IndexByte(key, '/'); i >= 0; i = nextSlash(key, i) {
			if leaves[key[:i]] {
				return nil, fmt.Errorf("policy: auth_policy_request_attributes %q is both a value and an object", key[:i])
			}
		}
	}
	return out, nil
}

func nextSlash(s string, i int) int {
	j := strings.IndexByte(s[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// templateParams is the expansion of one request: its variables, and the
// passdb: and userdb: fields, each value JSON-escaped where it lands.
func templateParams(vars, passdb, userdb map[string]string) *expand.Params {
	field := func(m map[string]string) expand.Lookup {
		return func(name string) (string, error) { return m[name], nil }
	}
	return &expand.Params{
		Table:          vars,
		Providers:      map[string]expand.Lookup{"passdb": field(passdb), "userdb": field(userdb)},
		Escape:         func(s string) (string, error) { return jsonEscape(s), nil },
		EscapeLiterals: true,
	}
}

// body is the JSON request body: the attributes in key order, nested by
// their paths, then the outcome where it applies and tls.
func (c *Client) body(req Request, pwhash string, includeStatus, success, policyReject bool) []byte {
	vars := make(map[string]string, len(VariableNames))
	src := req.Vars
	if len(src) == 0 {
		src = requestVars(req)
	}
	for _, n := range VariableNames {
		vars[n] = src[n]
	}
	vars["requested_username"] = req.Username
	vars["hashed_password"] = pwhash
	vars["fail_type"] = req.FailType
	params := templateParams(vars, req.Passdb, req.Userdb)

	var b strings.Builder
	b.WriteByte('{')
	var open []string
	first := true
	for _, a := range c.attrs {
		common := 0
		for common < len(open) && common < len(a.path)-1 && open[common] == a.path[common] {
			common++
		}
		for len(open) > common {
			b.WriteByte('}')
			open = open[:len(open)-1]
		}
		for _, obj := range a.path[len(open) : len(a.path)-1] {
			if !first {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%s":{`, jsonEscape(obj))
			open = append(open, obj)
			first = true
		}
		value, err := a.prog.Execute(params)
		if err != nil {
			slog.Warn("auth/policy: attribute not expanded", "key", strings.Join(a.path, "/"), "err", err)
			value = ""
		}
		if !first {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%s":"%s"`, jsonEscape(a.path[len(a.path)-1]), value)
		first = false
	}
	for range open {
		b.WriteByte('}')
	}
	sep := func() {
		if !first {
			b.WriteByte(',')
		}
		first = false
	}
	if includeStatus {
		sep()
		fmt.Fprintf(&b, `"success":%t`, success)
		sep()
		fmt.Fprintf(&b, `"policy_reject":%t`, policyReject)
	}
	sep()
	fmt.Fprintf(&b, `"tls":%t}`, req.TLS)
	return []byte(b.String())
}

// requestVars fills the table from a request's own fields, for a caller that
// built none.
func requestVars(req Request) map[string]string {
	return map[string]string{
		"user": req.Username, "original_user": req.Username, "auth_user": req.Username,
		"owner_user": req.Username, "remote_ip": req.RemoteIP, "real_remote_ip": req.RemoteIP,
		"local_ip": req.LocalIP, "real_local_ip": req.LocalIP, "protocol": sqlvars.ProtocolName(req.Service),
		"session": req.SessionID, "client_id": req.DeviceID, "local_port": "0", "remote_port": "0",
		"real_local_port": "0", "real_remote_port": "0", "client_pid": "0",
	}
}

// jsonEscape escapes a string for a JSON string literal the way the
// reference does: invalid UTF-8 becomes U+FFFD, U+2028 and U+2029 are escaped.
func jsonEscape(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && n <= 1 {
			b.WriteRune(utf8.RuneError)
			s = s[1:]
			continue
		}
		switch {
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x2028 || r == 0x2029:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[:n])
		}
		s = s[n:]
	}
	return b.String()
}
