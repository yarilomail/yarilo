// Package sqlvars is the variable grammar of the SQL passdb and userdb queries:
// which variables a query may name, and how they become bound parameters.
package sqlvars

import (
	"fmt"
	"strings"
)

// Kind is the query a variable appears in; each sees a different request.
type Kind int

const (
	// Password is a passdb password_query: the whole login request.
	Password Kind = iota
	// User is a userdb user_query: USER carries the protocol only (#2227).
	User
	// Iterate lists users and has no request at all.
	Iterate
)

func (k Kind) String() string {
	switch k {
	case Password:
		return "password_query"
	case User:
		return "user_query"
	default:
		return "iterate_query"
	}
}

// Vars are the request values a query may bind besides the username.
type Vars struct {
	Service  string // the internal protocol name; %s binds ProtocolName of it
	RemoteIP string
	LocalIP  string
}

// variable is one name a query may use.
type variable int

const (
	varUser variable = iota
	varLocal
	varDomain
	varProtocol
	varRemoteIP
	varLocalIP
)

var short = map[byte]variable{
	'u': varUser, 'n': varLocal, 'd': varDomain,
	's': varProtocol, 'r': varRemoteIP, 'l': varLocalIP,
}

var long = map[string]variable{
	"user": varUser, "username": varLocal, "domain": varDomain,
	"protocol": varProtocol, "service": varProtocol,
	"remote_ip": varRemoteIP, "rip": varRemoteIP,
	"local_ip": varLocalIP, "lip": varLocalIP,
}

// allowed says whether a query of kind k can be given v.
func allowed(k Kind, v variable) bool {
	switch k {
	case Password:
		return true
	case User:
		return v <= varProtocol
	default:
		return false
	}
}

// ProtocolName is the value %s binds: the reference's names, so a migrated
// query compares against what it always did (D3, #2227).
func ProtocolName(service string) string {
	switch service {
	case "smtp":
		return "submission"
	case "managesieve":
		return "sieve"
	}
	return service
}

// token is one variable occurrence: where it starts, how long it is.
type token struct {
	v      variable
	at, ln int
	name   string // the source spelling, for errors
}

// scan finds every variable outside quotes. An unknown %{name} is an error;
// an unknown short form is left to SQL, where % is also an operator.
func scan(query string) ([]token, error) {
	var out []token
	inQuote := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			inQuote = !inQuote
			continue
		}
		if inQuote || ch != '%' || i+1 >= len(query) {
			continue
		}
		if query[i+1] == '{' {
			end := strings.IndexByte(query[i+2:], '}')
			if end < 0 {
				return nil, fmt.Errorf("sqlvars: unterminated %%{ at offset %d", i)
			}
			name := query[i+2 : i+2+end]
			v, ok := long[name]
			if !ok {
				return nil, fmt.Errorf("sqlvars: unknown variable %%{%s}", name)
			}
			out = append(out, token{v: v, at: i, ln: end + 3, name: "%{" + name + "}"})
			i += end + 2
			continue
		}
		if v, ok := short[query[i+1]]; ok {
			out = append(out, token{v: v, at: i, ln: 2, name: query[i : i+2]})
			i++
		}
	}
	return out, nil
}

// Check refuses a query that names a variable its kind is never given: bound
// as empty, a protocol rule would fall into its ELSE branch and allow.
func Check(k Kind, query string) error {
	toks, err := scan(query)
	if err != nil {
		return err
	}
	for _, t := range toks {
		if !allowed(k, t.v) {
			return fmt.Errorf("sqlvars: %s cannot use %s: the %s request does not carry it", k, t.name, k)
		}
	}
	return nil
}

// UsesRequest reports whether the query names a value beyond the username.
func UsesRequest(query string) bool {
	toks, _ := scan(query)
	for _, t := range toks {
		if t.v >= varProtocol {
			return true
		}
	}
	return false
}

// UsesAddresses reports whether the query names the client's or the local
// address.
func UsesAddresses(query string) bool {
	toks, _ := scan(query)
	for _, t := range toks {
		if t.v == varRemoteIP || t.v == varLocalIP {
			return true
		}
	}
	return false
}

// Substitute turns each variable outside quotes into a placeholder (? or $N) and
// returns the values to bind: never SQL text, so no injection surface.
func Substitute(driver, query, username string, v Vars) (string, []any) {
	toks, err := scan(query)
	if err != nil {
		// Check runs at load; an unscannable query binds nothing.
		return query, nil
	}
	local, domain := SplitUser(username)
	val := func(x variable) string {
		switch x {
		case varUser:
			return username
		case varLocal:
			return local
		case varDomain:
			return domain
		case varProtocol:
			return ProtocolName(v.Service)
		case varRemoteIP:
			return v.RemoteIP
		default:
			return v.LocalIP
		}
	}
	var sb strings.Builder
	sb.Grow(len(query))
	var args []any
	last := 0
	for n, t := range toks {
		sb.WriteString(query[last:t.at])
		if driver == "postgres" {
			fmt.Fprintf(&sb, "$%d", n+1)
		} else {
			sb.WriteByte('?')
		}
		args = append(args, val(t.v))
		last = t.at + t.ln
	}
	sb.WriteString(query[last:])
	return sb.String(), args
}

// SplitUser splits user@domain; a name without @ has an empty domain.
func SplitUser(u string) (local, domain string) {
	if i := strings.IndexByte(u, '@'); i >= 0 {
		return u[:i], u[i+1:]
	}
	return u, ""
}
