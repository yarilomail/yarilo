package expand

import (
	"bytes"
	"crypto/aes"
	"encoding/hex"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

type expandCase struct {
	in, out string
	fail    bool // out is then a part of the error
}

func runCases(t *testing.T, p *Params, cases []expandCase) {
	t.Helper()
	for _, tc := range cases {
		got, err := Expand(tc.in, p)
		switch {
		case tc.fail && err == nil:
			t.Errorf("%q = %q, want an error with %q", tc.in, got, tc.out)
		case tc.fail && !strings.Contains(err.Error(), tc.out):
			t.Errorf("%q: error %q, want it to contain %q", tc.in, err, tc.out)
		case !tc.fail && err != nil:
			t.Errorf("%q: %v", tc.in, err)
		case !tc.fail && got != tc.out:
			t.Errorf("%q = %q, want %q", tc.in, got, tc.out)
		}
	}
}

func ok(in, out string) expandCase    { return expandCase{in: in, out: out} }
func fails(in, err string) expandCase { return expandCase{in: in, out: err, fail: true} }

func TestBuiltinFilters(t *testing.T) {
	p := &Params{Table: map[string]string{
		"first": "hello", "second": "world", "third": "pointer", "pointer": "portal",
		"port": "143", "three": "3", "encoded": "68656c6c6f", "domain": "test.example.org",
		"user": "user@test@domain", "multivalue": "one\ttwo\tthree", "uidvalidity": "1727121943",
		"empty": "", "\xce\xb8": "is theta", "null": "",
	}}
	runCases(t, p, []expandCase{
		ok("%{}", ""),
		fails("%{", "expecting CCBRACE or PIPE or NAME"),
		fails(`%{\`, "Premature program end"),
		ok("hello%{}world", "helloworld"),
		ok("%{first}", "hello"),
		ok("%{lookup('first')}", "hello"),
		ok("%{literal('hello')}", "hello"),
		fails("%{lookup}", "lookup: Missing name to lookup"),
		fails("%{literal}", "literal: Missing parameters"),
		ok("%{third}", "pointer"),
		ok("%{lookup(third)}", "portal"),
		fails("%{lookup(missing)}", "lookup: Unknown variable 'missing'"),
		ok("%{missing | default}", ""),
		ok("%{missing | default(first)}", "hello"),
		ok("%{missing | default('hello')}", "hello"),
		fails("%{missing | default(missing)}", "Unknown variable 'missing'"),
		fails("%{missing | upper}", "Unknown variable 'missing'"),
		fails("%{missing | default(missing) | upper}", "Unknown variable 'missing'"),
		ok("%{missing | default(missing) | default}", ""),
		ok("%{missing | default(missing) | default('none')}", "none"),
		ok("%{missing | default(missing) | default(first)}", "hello"),
		ok("%{missing | upper | default('none')}", "none"),
		ok("%{first | default(second)}", "hello"),
		ok("%{first | default('world')}", "hello"),
		ok("%{default(first)}", "hello"),
		fails("%{default(missing)}", "default: Unknown variable 'missing'"),
		ok("%{empty | default('nonempty')}", "nonempty"),
		ok("%{null}", ""),
		fails("%{missing}", "Unknown variable 'missing'"),
		ok("%{first | upper}", "HELLO"),
		ok("%{first | upper | lower}", "hello"),
		ok("%{first | substr(0)}", "hello"),
		ok("%{first | substr(5, 0)}", ""),
		ok("%{first | substr(1, 2)}", "el"),
		ok("%{first | substr(-2, 2)}", "lo"),
		ok("%{first | substr(2, -2)}", "l"),
		ok("%{first | substr(-1, -1)}", ""),
		ok("%{first | substr(0, -1)}", "hell"),
		ok("%{first | substr(-1)}", "o"),
		ok("%{first | substr(-5)}", "hello"),
		fails("%{first | substr(6)}", "substr: Offset out of bounds"),
		fails("%{first | substr(-6)}", "substr: Offset out of bounds"),
		fails("%{first | substr(1, 5)}", "substr: Length out of bounds"),
		fails("%{first | substr(1, -5)}", "substr: Length out of bounds"),
		fails("%{first | substr(-1, 5)}", "substr: Length out of bounds"),
		fails("%{first | substr(-1, -5)}", "substr: Length out of bounds"),
		fails("%{first | substr(5, 1)}", "substr: Length out of bounds"),
		fails("%{first | substr(5, -1)}", "substr: Length out of bounds"),
		ok("%{first | substr(-5, 1)}", "h"),
		ok("%{first | substr(-5, -1)}", "hell"),
		fails("%{first | substr(-6, 1)}", "substr: Offset out of bounds"),
		fails("%{first | substr(-6, -1)}", "substr: Offset out of bounds"),
		fails("%{substr}", "substr: Missing parameters"),
		fails("%{substr(0,0)}", "substr: No value to substring"),
		ok("%{first | reverse}", "olleh"),
		fails("%{reverse}", "reverse: No value to reverse"),
		ok("%{first | concat(' ',second)}", "hello world"),
		ok("%{concat(first,' ',second)}", "hello world"),
		fails("%{concat}", "Missing parameters"),
		ok("%{first | sha1}", "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"),
		ok("%{first | sha1(rounds=1000)}", "c0ddea212ee1af8d6401947d29c8bfab31f8ad93"),
		ok("%{first | sha1(salt='world')}", "5715790a892990382d98858c4aa38d0617151575"),
		ok("%{first | sha1(rounds=1000,salt='world')}", "a314ec3ef5103223a4c5bd285dbe4ea5534d4334"),
		ok("%{literal('1724925643') | hex}", "66d046cb"),
		fails("%{hash}", "hash: Missing parameters"),
		fails("%{hash(rounds=1000)}", "hash: No algorithm as first parameter"),
		fails("%{hash('md5',fail=1)}", "hash: Unsupported key 'fail'"),
		fails("%{md5(fail=1)}", "md5: Unsupported key 'fail'"),
		ok("%{encoded | unhexlify | text}", "hello"),
		ok("%{encoded | unhexlify}", "68656c6c6f"),
		ok("%{three | hexlify(4)}", "0033"),
		fails("%{hexlify(fail=1)}", "hexlify: Unsupported key 'fail'"),
		fails("%{encoded | hexlify(900)}", "hexlify: Excessive padding"),
		ok("%{three | hex | unhex}", "3"),
		ok("%{uidvalidity | hex | unhex}", "1727121943"),
		ok("%{three | hex(2)}", "03"),
		ok("%{uidvalidity | hex(2)}", "17"),
		ok("%{uidvalidity | hex(-2)}", "66"),
		ok("%{uidvalidity | hex }", "66f1ca17"),
		fails("%{uidvalidity | hex(257) }", "hex: Excessive padding"),
		fails("%{uidvalidity | hex(-257) }", "hex: Excessive padding"),
		ok("%{first | base64}", "aGVsbG8="),
		ok("%{first | base64 | unbase64 | text}", "hello"),
		fails("%{base64(0)}", "base64: Too many positional parameters"),
		fails("%{base64(fail=1)}", "base64: Unsupported key 'fail'"),
		fails("%{unbase64(0)}", "unbase64: Too many positional parameters"),
		fails("%{unbase64(fail=1)}", "unbase64: Unsupported key 'fail'"),
		ok("%{first | base64(pad=0)}", "aGVsbG8"),
		fails("%{first | base64(pad='yes')}", "base64: 'yes' is not a number"),
		fails("%{first | base64(pad=-5)}", "base64: '-5' is not 0 or 1"),
		ok("%{literal('<<???>>') | base64(url=1)}", "PDw_Pz8-Pg=="),
		ok("%{literal('<<???>>') | base64(pad=0,url=1)}", "PDw_Pz8-Pg"),
		ok("%{first | truncate(4)}", "hell"),
		ok("%{first | truncate(5)}", "hello"),
		ok("%{first | truncate(6)}", "hello"),
		ok("%{first | truncate(three)}", "hel"),
		ok("%{first | truncate(bits=7)}", "4"),
		fails("%{truncate}", "truncate: Missing parameter"),
		fails("%{truncate('hello')}", "truncate: 'hello' is not a number"),
		fails("%{truncate(first)}", "truncate: 'hello' (in first) is not a number"),
		fails("%{truncate(3)}", "truncate: No value to truncate"),
		ok("cn=%{first},ou=%{domain | ldap_dn}", "cn=hello,ou=test,dc=example,dc=org"),
		ok("%{literal('hello world') | regexp('(.*) (.*)', '$2 $1')}", "world hello"),
		ok("%{literal('hello world') | regexp('(.*) (.*)', '$2x')}", "worldx"),
		ok("%{literal('abc') | regexp('x*', 'Y')}", "abc"),
		ok("%{literal('abc') | regexp('b*', 'Y')}", "aYc"),
		ok(`%{literal('hello world') | regexp('(.*) (.*)', '\\2 \\1')}`, "world hello"),
		ok(`%{literal('hello world') | regexp('(.*) (.*)', '\\\\2 \\\\1')}`, `\2 \1`),
		ok(`%{literal('hello world') | regexp('(.*) (.*)', '\\\\\\2 \\\\\\1')}`, `\world \hello`),
		ok("%{user | index('@',0)}", "user"),
		ok("%{user | username}", "user"),
		ok("%{user | domain}", "test@domain"),
		ok("%{user | domain | domain}", "domain"),
		ok("%{user | index('@',1)}", "test"),
		ok("%{user | index('@',2)}", "domain"),
		fails("%{user | index('@',3)}", "index: Position out of bounds"),
		fails("%{user | index('@',-4)}", "index: Position out of bounds"),
		fails("%{user | index('@',-5)}", "index: Position out of bounds"),
		ok("%{user | index('@',-4) | default('hello')}", "hello"),
		ok("%{user | index('@',-3)}", "user"),
		ok("%{user | index('@',-2)}", "test"),
		ok("%{user | index('@',-1)}", "domain"),
		fails("%{user | username(0)}", "username: Too many positional parameters"),
		fails("%{user | domain(0)}", "domain: Too many positional parameters"),
		fails("%{user | index('',0)}", "Empty separator"),
		ok("%{literal('hello@') | domain }", ""),
		ok("%{literal('@hello') | username }", ""),
		ok("%{literal('@') | domain }", ""),
		ok("%{literal('@') | username }", ""),
		ok("%{literal('') | username }", ""),
		ok("%{literal('') | domain }", ""),
		ok("%{literal('username') | username }", "username"),
		ok("%{literal('username') | domain }", ""),
		ok("%{multivalue | list(',')}", "one,two,three"),
		ok("%{literal('1') | rfill(3)}", "100"),
		ok("%{literal('1') | lfill(3)}", "001"),
		ok("%{literal('1') | lfill(3, ' ')}", "  1"),
		ok("%{first | md5 | hexlify(8)}", "5d41402a"),
		ok("%{first | md5 | substr(4,4) }", "bc4b2a76"),
		ok("%{first | md5 | substr(0,8) % 256 | hex}", "76"),
		ok("%{first | md5 | substr(0,8) % 30 | hex }", "c"),
		ok("%{first | md5 % 256 | hex}", "92"),
		ok("%{first | sha512 % 256 | hex }", "43"),
		ok("%{first | md5 % 30 | hex }", "16"),
		ok("%{first | md5 % 30 | hex(2) }", "16"),
		ok("%{first | md5 % 30 | hex(4) }", "0016"),
		ok("%{first | md5 % 30 | hex(-4) }", "1600"),
		ok("%{\xce\xb8}", "is theta"),
		fails("%{\xff\xfe\xff}", "Invalid UTF-8 string"),
		fails("%{first | escape}", "No escape function available"),
	})
}

func TestMath(t *testing.T) {
	p := &Params{Table: map[string]string{
		"first": "1", "second": "2", "third": "pointer", "pointer": "4", "port": "143", "nan": "nanana",
	}}
	runCases(t, p, []expandCase{
		ok("%{literal('1') + 1}", "2"),
		ok("%{first + 1}", "2"),
		ok("%{first * 10}", "10"),
		ok("%{lookup(third) / 2}", "2"),
		fails("%{nan - 1}", "Input is not a number"),
		ok("%{literal('31') | unhexlify | text * 5}", "5"),
		ok("%{literal('31') | unhex * 5}", "245"),
		ok("%{port % 5}", "3"),
		fails("%{port / 0}", "calculate: Division must be by positive integer"),
		fails("%{port % 0}", "calculate: Modulo must be by positive integer"),
		fails("%{port / -1}", "calculate: Division must be by positive integer"),
		fails("%{port % -1}", "calculate: Modulo must be by positive integer"),
		fails("%{literal('-9223372036854775808') / -1}", "calculate: Division must be by positive integer"),
		fails("%{literal('-9223372036854775808') % -1}", "calculate: Modulo must be by positive integer"),
		fails("%{third | sha256 % 0}", "calculate: Binary modulo must be positive integer"),
		fails("%{failure} %{port}", "Unknown variable 'failure'"),
		fails("%{port} %{failure}", "Unknown variable 'failure'"),
	})
}

func TestIf(t *testing.T) {
	p := &Params{Table: map[string]string{
		"alpha": "alpha", "beta": "beta", "one": "1", "two": "2", "evil1": ";', ':", "evil2": ";test;",
	}}
	runCases(t, p, []expandCase{
		ok("%{if(1, '==', 1, 'yes', 'no')}", "yes"),
		ok("%{literal('1') | if('==', 2, 'yes', 'no')}", "no"),
		ok("%{literal('1') | if('<', 1, 'yes', 'no')}", "no"),
		ok("%{literal('1') | if('<', 2, 'yes', 'no')}", "yes"),
		ok("%{literal('1') | if('<=', 1, 'yes', 'no')}", "yes"),
		ok("%{literal('1') | if('<=', 2, 'yes', 'no')}", "yes"),
		ok("%{literal('1') | if('>', 1, 'yes', 'no')}", "no"),
		ok("%{literal('1') | if('>', 2, 'yes', 'no')}", "no"),
		ok("%{literal('1') | if('>=', 1, 'yes', 'no')}", "yes"),
		ok("%{literal('1') | if('>=', 2, 'yes', 'no')}", "no"),
		ok("%{literal('1') | if('!=', 1, 'yes', 'no')}", "no"),
		ok("%{literal('1') | if('!=', 2, 'yes', 'no')}", "yes"),
		ok("%{if('a', 'eq', 'a', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('eq', 'b', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('lt', 'a', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('lt', 'b', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('le', 'a', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('le', 'b', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('gt', 'a', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('gt', 'b', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('ge', 'a', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('ge', 'b', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('ne', 'a', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('ne', 'b', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('*', 'a', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('*', 'b', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('*', '*a*', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('*', '*b*', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('*', '*', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('!*', 'a', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('!*', 'b', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('!*', '*a*', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('!*', '*b*', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('!*', '*', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('~', 'a', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('~', 'b', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('~', '.*a.*', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('~', '.*b.*', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('~', '.*', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('!~', 'a', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('!~', 'b', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('!~', '.*a.*', 'yes', 'no')}", "no"),
		ok("%{literal('a') | if('!~', '.*b.*', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('!~', '.*', 'yes', 'no')}", "no"),
		ok("%{literal('this is test') | if('~', '^test', 'yes', 'no')}", "no"),
		ok("%{literal('this is test') | if('~', '.*test', 'yes', 'no')}", "yes"),
		ok("%{literal('a') | if('~', 'x*', 'yes', 'no')}", "no"),
		ok("%{alpha | if('eq', alpha, 'yes', 'no')}", "yes"),
		ok("%{alpha | if('eq', beta, 'yes', 'no')}", "no"),
		ok("%{one | if('eq', one, 'yes', 'no')}", "yes"),
		ok("%{one | if('eq', two, 'yes', 'no')}", "no"),
		ok("%{one | if('eq', one, one, two)}", "1"),
		ok("%{one | if('gt', two, one, two)}", "2"),
		ok(`%{evil1 | if('eq', ';\', \':', evil2, 'no')}`, ";test;"),
		fails("%{if('gt', two, one, two)}", "if: Missing parameters"),
		fails("%{if(1, '', 1, 'yes', 'no')}", "if: Unsupported comparator ''"),
		fails("%{if(missing, '==', 1, 'yes', 'no')}", "if: Left-hand side: Unknown variable 'missing'"),
		fails("%{if(1, missing, 1, 'yes', 'no')}", "if: Comparator: Unknown variable 'missing'"),
		fails("%{if(1, '==', missing, 'yes', 'no')}", "if: Right-hand side: Unknown variable 'missing'"),
		fails("%{if(1, '==', 1, missing, 'no')}", "if: True branch: Unknown variable 'missing'"),
		fails("%{if(1, '==', 0, 'yes', missing)}", "if: False branch: Unknown variable 'missing'"),
		fails("%{if(1, '==', 1, 'yes', 'no', 'maybe')}", "if: Too many positional parameters"),
		fails("%{if(fail=1)}", "if: Unsupported key 'fail'"),
		fails("%{alpha|if('==', two, one, two)}", "if: Input is not a number"),
	})
}

func TestSwitch(t *testing.T) {
	p := &Params{Table: map[string]string{
		"alpha": "alpha", "beta": "beta", "one": "1", "two": "2", "evil2": ";test;",
	}}
	runCases(t, p, []expandCase{
		ok(`%{switch(alpha, "eq", alpha, "yes", beta, "no", one, "other", two)}`, "yes"),
		ok(`%{beta | switch("eq", alpha, "yes", beta, "no", one, "other", two)}`, "no"),
		ok(`%{one | switch("eq", alpha, "yes", beta, "no", one, "other", two)}`, "other"),
		ok(`%{evil2 | switch("eq", alpha, "yes", beta, "no", one, "other", two)}`, "2"),
		ok(`%{one | switch("==", 1, 2, 2, 3, 3, 4, -1)}`, "2"),
		fails(`%{literal('0') | switch("==", 1, 2, 2, 3, 3, 4)}`, "switch: No default value provided"),
		fails(`%{literal('0') | switch("==", 1, 2, 2, 3, 3, 4, four)}`, "switch: in condition #3: Unknown variable 'four'"),
		ok(`%{literal('0') | switch("==", 1, 2, 2, 3, 3, 4) | default}`, ""),
		fails(`%{one | switch("==", 1)}`, "switch: At least one condition-value pair is required"),
		fails(`%{switch}`, "switch: Missing parameters"),
		fails(`%{switch("==")}`, "switch: Missing parameters"),
		fails(`%{switch(alpha, "==")}`, "switch: At least one condition-value pair is required"),
		fails(`%{alpha | switch("==")}`, "switch: At least one condition-value pair is required"),
		fails(`%{switch(alpha, "eq", "default")}`, "switch: At least one condition-value pair is required"),
	})
}

// testEscape quotes a value the way an SQL string escape would.
func testEscape(s string) (string, error) {
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'", nil
}

func TestEscape(t *testing.T) {
	p := &Params{Escape: testEscape, Table: map[string]string{
		"clean": "hello world", "escape": `'hello' "world"`, "first": "bobby",
		"nasty": "';-- SELECT * FROM bobby.tables", "feisty": "' OR '1'='1",
	}}
	runCases(t, p, []expandCase{
		ok("%{clean}", "'hello world'"),
		ok("%{escape}", `'\'hello\' "world"'`),
		ok("SELECT * FROM bobby.tables WHERE name = %{first}", "SELECT * FROM bobby.tables WHERE name = 'bobby'"),
		ok("SELECT * FROM bobby.tables WHERE name = %{nasty}", `SELECT * FROM bobby.tables WHERE name = '\';-- SELECT * FROM bobby.tables'`),
		ok("no variables", "no variables"),
		ok("%{literal('hello')}", "'hello'"),
		ok(`hello\tworld`, `hello\tworld`),
		ok("%%%%%%%%%%%%%%%%%%", "%%%%%%%%%%%%%%%%%%"),
		ok("%{literal('hello\r\n\tworld')}", "'hello\r\n\tworld'"),
		ok(`\110\145\154\154\157`, `\110\145\154\154\157`),
		ok(`%{literal('\110\145\154\154\157')}`, "'Hello'"),
		ok(`\x20\x21`, `\x20\x21`),
		ok(`%{literal('\x20\x21')}`, "' !'"),
		ok(`\\x20`, `\\x20`),
		ok(`%{literal('\\x20')}`, `'\x20'`),
		ok(`\xgg`, `\xgg`),
		fails(`%{literal('\xgg')}`, "Invalid character escape"),
		ok(`\999`, `\999`),
		fails(`%{literal('\999')}`, "Invalid character escape"),
		ok("%{literal('one\ttwo\tthree') | list}", "'one,two,three'"),
		ok(`\hello\world`, `\hello\world`),
		ok(`%{literal('\'\\hello\\world\'')}`, `'\'\hello\world\''`),
		ok(`%{literal("\"\\hello\\world\"")}`, `'"\hello\world"'`),
		fails(`%{literal('\z')}`, "Invalid character escape"),
		fails(strings.Repeat("0123456789", 820), "Program size exceeds maximum of 8192 bytes"),
		ok("%{feisty}", `'\' OR \'1\'=\'1'`),
		ok("%{clean|safe} and %{feisty}", `hello world and '\' OR \'1\'=\'1'`),
		fails("%{clean|safe|upper}", "safe filter must be last in the filter chain"),
		ok("%{clean | escape}", `'\'hello world\''`),
		ok("%{clean | escape | safe}", "'hello world'"),
		ok("%{clean | escape | concat(' world') | safe}", "'hello world' world"),
	})
}

func TestPercent(t *testing.T) {
	p := &Params{Table: map[string]string{"test": "value"}}
	runCases(t, p, []expandCase{
		ok("%%{test}", "%{test}"),
		ok("%Lu", "%Lu"),
		ok("%%Lu", "%%Lu"),
		ok("%{test}", "value"),
		ok("%%", "%%"),
		ok("%", "%"),
		ok("%%-%%{test}-%%{test}", "%%-%{test}-%{test}"),
	})
}

func TestTimestamps(t *testing.T) {
	p := &Params{Table: map[string]string{
		"ts": "1749379200", "tsfrac": "1749379200.123456789", "tsms": "1700000000.5", "epoch": "0",
		"bigsec": "10000000000", "tslong": "1749379200.1234567891", "ms": "1700000000500",
		"ns": "1749379200123456789", "word": "hello",
	}}
	runCases(t, p, []expandCase{
		ok("%{ts | epoch}", "1749379200"),
		ok("%{ts | epoch('s')}", "1749379200"),
		ok("%{tsfrac | epoch}", "1749379200"),
		ok("%{tsfrac | epoch('ms')}", "1749379200123"),
		ok("%{tsfrac | epoch('us')}", "1749379200123456"),
		ok("%{tsfrac | epoch('ns')}", "1749379200123456789"),
		ok("%{tslong | epoch('ns')}", "1749379200123456789"),
		ok("%{tsms | epoch('ms')}", "1700000000500"),
		ok("%{tsms | epoch('ns')}", "1700000000500000000"),
		ok("%{tsfrac | epoch(unit='ms')}", "1749379200123"),
		ok("%{epoch | epoch('ns')}", "0"),
		ok("%{bigsec | epoch('s')}", "10000000000"),
		fails("%{bigsec | epoch('ns')}", "out of range for unit 'ns'"),
		fails("%{ts | epoch('bogus')}", "Unsupported unit 'bogus' for 'epoch'"),
		fails("%{word | epoch('s')}", "Invalid timestamp 'hello'"),
		fails("%{ts2 | epoch('s')}", "Unknown variable 'ts2'"),
		ok("%{ts | from_epoch}", "1749379200.000000000"),
		ok("%{ms | from_epoch('ms')}", "1700000000.500000000"),
		ok("%{ns | from_epoch('ns')}", "1749379200.123456789"),
		ok("%{ms | from_epoch(unit='ms')}", "1700000000.500000000"),
		ok("%{ns | from_epoch('ns') | epoch('ns')}", "1749379200123456789"),
		fails("%{ms | from_epoch('bogus')}", "Unsupported unit 'bogus' for 'from_epoch'"),
		fails("%{word | from_epoch('ms')}", "Invalid timestamp 'hello'"),
		fails("%{from_epoch('ms')}", "from_epoch: No value to convert from epoch"),
		ok("%{ts | date('%Y-%m-%d %H:%M:%S')}", "2025-06-08 10:40:00"),
		ok("%{ts | date('%Y-%m-%d %H:%M:%S', 'gmt')}", "2025-06-08 10:40:00"),
		ok("%{ts | date(format='%H:%M', tz='utc')}", "10:40"),
		ok("%{epoch | date('%Y-%m-%d %H:%M:%S')}", "1970-01-01 00:00:00"),
		ok("%{tsms | date('%Y-%m-%d %H:%M:%S')}", "2023-11-14 22:13:20"),
		ok("%{ms | from_epoch('ms') | date('%Y-%m-%d %H:%M:%S')}", "2023-11-14 22:13:20"),
		fails("%{ts | date}", "date: Missing date format"),
		fails("%{ts | date('%H', 'mars')}", "Unsupported timezone 'mars' for 'date'"),
		fails("%{date('%H')}", "date: No value to format as date"),
		ok("%{ts | date('%Y') | upper}", "2025"),
		ok("%{ts | iso8601}", "2025-06-08T10:40:00Z"),
		ok("%{ts | iso8601(tz='gmt')}", "2025-06-08T10:40:00Z"),
		ok("%{tsms | iso8601}", "2023-11-14T22:13:20Z"),
		fails("%{ts | iso8601('mars')}", "Unsupported timezone 'mars' for 'iso8601'"),
		fails("%{iso8601}", "iso8601: No value to format as ISO 8601"),
	})
	got, err := Expand("%{time:unix}", p)
	if err != nil || !regexp.MustCompile(`^\d+\.\d{9}$`).MatchString(got) {
		t.Errorf("time:unix = %q, %v", got, err)
	}
}

func TestProviders(t *testing.T) {
	t.Setenv("YARILO_EXPAND_TEST", "set")
	p := &Params{Providers: map[string]Lookup{"custom": func(k string) (string, error) {
		if k == "value" {
			return "test", nil
		}
		return "", errUnsupportedKey(k)
	}}}
	runCases(t, p, []expandCase{
		ok("%{process:pid}", strconv.Itoa(os.Getpid())),
		ok("%{env:YARILO_EXPAND_TEST}", "set"),
		ok("%{env:YARILO_EXPAND_MISSING}", ""),
		fails("%{invalid:whatever}", "Unsupported prefix 'invalid'"),
		ok("%{custom:value}", "test"),
		fails("%{custom:null}", "custom: Unsupported key 'null'"),
		fails("%{event:anything}", "No event available"),
		ok("%{event:anything|default}", ""),
		fails("%{process:nope}", "process: Unsupported field 'nope'"),
		ok("%{system:cpu_count|if('>', 0, 'some', 'none')}", "some"),
	})
	for _, tc := range []struct{ key, re string }{
		{"guid128", `^[0-9a-f]{32}$`},
		{"uuid", `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`},
		{"uuid:record", `^[0-9a-f-]{36}$`},
		{"uuid:compact", `^[0-9a-f]{32}$`},
		{"uuid:microsoft", `^\{[0-9a-f-]{36}\}$`},
	} {
		got, err := Expand("%{generate:"+tc.key+"}", nil)
		if err != nil || !regexp.MustCompile(tc.re).MatchString(got) {
			t.Errorf("generate:%s = %q, %v", tc.key, got, err)
		}
	}
	host, _ := os.Hostname()
	if got, _ := Expand("%{generate:guid}", nil); !strings.HasSuffix(got, "."+host) {
		t.Errorf("generate:guid = %q, want the host name in it", got)
	}
}

func TestCrypt(t *testing.T) {
	p := &Params{Table: map[string]string{
		"iv": "98b3b40a48ca40f998b3b40a48ca40f9", "key": "cc2981c8f38aea59cc2981c8f38aea59",
		"encrypted_raw": "46b58741763fe22598014be26331a082",
		"encrypted":     "98b3b40a48ca40f998b3b40a48ca40f9$46b58741763fe22598014be26331a082$",
		"decrypted":     "hello, world", "user": "foo",
		"salt-encrypted":  "s=foo,r=1000$da793a4ae62eb1d415228b40c43b93a8$",
		"ecb-encrypted":   "$3bca3caa565f2d940acef1244a07c836$",
		"trunckey":        "cc2981c8f38aea59cc2981c8f38aea5",
		"invalidkey":      "invalid",
		"trunciv":         "cc2981c8f38aea59cc2981c8f38aea5",
		"invalidiv":       "invalid",
		"encrypted-bad-1": "98b3b40a48ca40f998b3b40a48ca40fx$46b58741763fe22598014be26331a082$",
		"encrypted-bad-2": "98b3b40a48ca40f998b3b40a48ca40f9$46b58741763fe22598014be26331a08x$",
	}}
	runCases(t, p, []expandCase{
		fails("%{decrypted|encrypt(algorithm='null')}", ""),
		ok("%{decrypted|encrypt(algorithm='aes-128-cbc',iv=iv,key=key)}", "98b3b40a48ca40f998b3b40a48ca40f9$46b58741763fe22598014be26331a082$"),
		ok("%{decrypted|encrypt(algorithm='aes-128-cbc',iv=iv,key=key,raw=1)}", "46b58741763fe22598014be26331a082"),
		fails("%{encrypted|decrypt(key=key,algorithm='null')}", "Invalid cipher null"),
		ok("%{encrypted|decrypt(algorithm='aes-128-cbc',key=key)}", "hello, world"),
		ok("%{encrypted_raw|unhexlify|decrypt(algorithm='aes-128-cbc',iv=iv,key=key,raw=1)}", "hello, world"),
		ok("%{user|encrypt(hash='sha1',rounds=16,key='bar',salt='bar',algorithm='aes-256-cbc',raw=1)}", "92da344a4456e112550c146d9b3e4334"),
		ok("%{literal('foo.20161103001007.SMTP.SEND')|encrypt(hash='sha1',rounds=16,key='secret',salt='secret',algorithm='aes-256-cbc',raw=1)}",
			"c23ce33218c955a4e791742c6a8eb38717c9b2dd6cc1cf670dff029e819cca81"),
		ok("%{user|encrypt(key='bar',salt='foo')}", "s=foo,r=10000$929803f6a290ec6f015bf1a921aa352a$"),
		ok("%{salt-encrypted|decrypt(key='bar')}", "foo"),
		ok("%{user|encrypt(key='bar',salt='foo')|decrypt(key='bar')}", "foo"),
		ok("%{decrypted|encrypt(algorithm='aes-128-ecb',key=key,iv='')}", "$3bca3caa565f2d940acef1244a07c836$"),
		ok("%{ecb-encrypted|decrypt(algorithm='aes-128-ecb',key=key)}", "hello, world"),
		ok("%{decrypted|encrypt(algorithm='aes-128-ecb',key=key,iv='')|decrypt(key=key,algorithm='aes-128-ecb')}", "hello, world"),
		fails("%{decrypted|encrypt(algorithm='aes-128-cbc',key=trunckey,iv=iv)|decrypt(key=trunckey,algorithm='aes-128-cbc')}", "Invalid key"),
		fails("%{decrypted|encrypt(algorithm='aes-128-cbc',key=invalidkey,iv=iv)|decrypt(key=invalidkey,algorithm='aes-128-cbc')}", "Invalid key"),
		fails("%{decrypted|encrypt(algorithm='aes-128-cbc',key=key,iv=trunciv)|decrypt(key=key,algorithm='aes-128-cbc')}", "Invalid iv"),
		fails("%{decrypted|encrypt(algorithm='aes-128-cbc',key=key,iv=invalidiv)|decrypt(key=key,algorithm='aes-128-cbc')}", "Invalid iv"),
		fails("%{encrypted-bad-1|decrypt(algorithm='aes-128-cbc',key=key)}", "Invalid iv"),
		fails("%{encrypted-bad-2|decrypt(algorithm='aes-128-cbc',key=key)}", "Invalid input"),
	})
	// A random salt still decrypts with the same key.
	got, err := Expand("%{decrypted|encrypt(key='k')|decrypt(key='k')}", p)
	if err != nil || got != "hello, world" {
		t.Errorf("random-salt round trip = %q, %v", got, err)
	}
}

func TestParameterOrder(t *testing.T) {
	for _, in := range []string{
		"%{f(c='3', b='2', a='1', '1', '2', '3')}",
		"%{f(C='3', B='2', a='1', '1', '2', '3')}",
	} {
		prog, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		ps := prog.programs[0].stmts[0].params
		var got []string
		for _, p := range ps {
			if p.idx >= 0 {
				got = append(got, strconv.Itoa(p.idx))
			} else {
				got = append(got, p.key)
			}
		}
		if strings.Join(got, ",") != "0,1,2,a,b,c" {
			t.Errorf("%s: parameters %v, want positional first then keys in order", in, got)
		}
	}
}

func TestHashMethods(t *testing.T) {
	p := &Params{Table: map[string]string{"first": "hello"}}
	runCases(t, p, []expandCase{
		ok("%{first | hash('md4')}", "866437cb7a794bce2b727acc0362ee27"),
		ok("%{first | hash('sha3-256')}", "3338be694f50c5f338814986cdf0686453a888b84f424d792af4b9202398f392"),
		ok("%{first | hash('xxh64')}", "26c7827d889f6da3"),
		ok("%{first | hash('size')}", "0000000000000005"),
		fails("%{first | hash('nope')}", "Unsupported algorithm 'nope'"),
	})
}

// A plaintext whose last byte is 0 has no valid padding: decrypting it fails
// rather than handing back the block.
func TestDecryptRefusesAZeroPadByte(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 16)
	block, _ := aes.NewCipher(key)
	plain := append(bytes.Repeat([]byte{'a'}, 15), 0)
	ct := make([]byte, 16)
	block.Encrypt(ct, plain)
	p := &Params{Table: map[string]string{"ct": hex.EncodeToString(ct), "key": hex.EncodeToString(key)}}
	runCases(t, p, []expandCase{
		fails("%{ct | unhexlify | decrypt(algorithm='aes-128-ecb', key=key, iv='', raw=1)}", "bad decrypt"),
	})
}
