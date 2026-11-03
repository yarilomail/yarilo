package expand

import "testing"

// A filter that takes no parameters refuses one, positional or named, and a
// filter with nothing to work on says so, naming what it would have done.
func TestFiltersRefuseWhatTheyCannotUse(t *testing.T) {
	p := &Params{Table: map[string]string{"v": "abc"}}
	var cases []expandCase
	for _, f := range []string{"lower", "upper", "unhex", "unhexlify", "ldap_dn", "text", "reverse", "username", "domain"} {
		cases = append(cases,
			fails("%{v | "+f+"(1)}", "Too many positional parameters"),
			fails("%{v | "+f+"(k=1)}", "Unsupported key 'k'"),
			fails("%{"+f+"}", "No value to"),
		)
	}
	runCases(t, p, cases)
}

// The conversions refuse input they cannot read rather than guess.
func TestConversionsRefuseBadInput(t *testing.T) {
	p := &Params{Table: map[string]string{
		"hexnum": "ff", "nothex": "zz", "odd": "abc", "b64": "aGVsbG8", "dotted": "mail.example.org",
		"bytes": "hello",
	}}
	runCases(t, p, []expandCase{
		ok("%{hexnum | unhex}", "255"),
		fails("%{nothex | unhex}", "Input is not a hex number"),
		fails("%{odd | unhexlify}", "Not a hex value"),
		fails("%{nothex | unhexlify}", "Not a hex value"),
		ok("%{b64 | unbase64(pad=0)}", "68656c6c6f"),
		fails("%{b64 | unbase64(pad=2)}", "Supported values for pad are 0 or 1"),
		fails("%{b64 | unbase64(1)}", "Too many positional parameters"),
		ok("%{dotted | ldap_dn}", "mail,dc=example,dc=org"),
		ok("%{bytes | hexlify(4)}", "6865"),
		ok("%{bytes | hexlify(12)}", "0068656c6c6f"),
		fails("%{bytes | hexlify(-1)}", "Width must be positive"),
		fails("%{bytes | hexlify(w=1)}", "Unsupported key 'w'"),
		fails("%{literal(k='x')}", "Unsupported key 'k'"),
	})
}
