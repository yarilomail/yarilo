package expand

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxPadding bounds what hex and hexlify may add.
const maxPadding = 256

type filterFunc func(c *call) error

var filters = map[string]filterFunc{
	"lookup": fnLookup, "literal": fnLiteral, "calculate": fnCalculate, "concat": fnConcat,
	"upper": fnUpper, "lower": fnLower, "hash": fnHash,
	"md5": hashAs("md5"), "sha1": hashAs("sha1"), "sha256": hashAs("sha256"),
	"sha384": hashAs("sha384"), "sha512": hashAs("sha512"),
	"base64": fnBase64, "unbase64": fnUnbase64, "hex": fnHex, "unhex": fnUnhex,
	"hexlify": fnHexlify, "unhexlify": fnUnhexlify, "default": fnDefault, "reverse": fnReverse,
	"truncate": fnTruncate, "substr": fnSubstr, "ldap_dn": fnLdapDN, "if": fnIf,
	"regexp": fnRegexp, "lenumber": numberAs(binary.LittleEndian), "benumber": numberAs(binary.BigEndian),
	"index": fnIndex, "username": fnUsername, "domain": fnDomain, "list": fnList,
	"lfill": fillAs(true), "rfill": fillAs(false), "text": fnText,
	"encrypt": fnEncrypt, "decrypt": fnDecrypt, "switch": fnSwitch,
	"escape": fnEscape, "safe": fnSafe,
	"epoch": fnEpoch, "from_epoch": fnFromEpoch, "date": fnDate, "iso8601": fnISO8601,
}

func fnLookup(c *call) error {
	ps := c.params()
	if len(ps) == 0 {
		return errors.New("Missing name to lookup")
	}
	if ps[0].key != "" {
		return errUnsupportedKey(ps[0].key)
	}
	c.st.unset()
	name, err := c.str(&ps[0])
	if err != nil {
		return err
	}
	v, err := c.st.lookup(name)
	if err != nil {
		return err
	}
	c.st.setString(v)
	return nil
}

func fnLiteral(c *call) error {
	if err := c.needParams(); err != nil {
		return err
	}
	if k := c.params()[0].key; k != "" {
		return errUnsupportedKey(k)
	}
	v, err := c.any(&c.params()[0])
	if err != nil {
		return err
	}
	c.st.setString(v)
	return nil
}

func fnLower(c *call) error {
	if err := c.noParams(); err != nil {
		return err
	}
	if err := c.needTransfer("lower"); err != nil {
		return err
	}
	c.st.transfer = asciiCase(c.st.transfer, false)
	return nil
}

func fnUpper(c *call) error {
	if err := c.noParams(); err != nil {
		return err
	}
	if err := c.needTransfer("upper"); err != nil {
		return err
	}
	c.st.transfer = asciiCase(c.st.transfer, true)
	return nil
}

// asciiCase changes ASCII letters only, as the C library's case functions do.
func asciiCase(b []byte, upper bool) []byte {
	for i, ch := range b {
		switch {
		case upper && ch >= 'a' && ch <= 'z':
			b[i] = ch - 'a' + 'A'
		case !upper && ch >= 'A' && ch <= 'Z':
			b[i] = ch - 'A' + 'a'
		}
	}
	return b
}

func fnDefault(c *call) error {
	if c.st.set && len(c.st.transfer) > 0 {
		return nil
	}
	value := ""
	if ps := c.params(); len(ps) > 0 {
		if ps[0].key != "" {
			return errUnsupportedKey(ps[0].key)
		}
		v, err := c.any(&ps[0])
		if err != nil {
			return err
		}
		if len(ps) > 1 {
			return errTooManyPositional
		}
		value = v
	}
	c.st.setString(value)
	return nil
}

func fnCalculate(c *call) error {
	if err := c.needParams(); err != nil {
		return err
	}
	oper := -1
	var right int64
	for i := range c.params() {
		p := &c.params()[i]
		if p.key != "" {
			return errUnsupportedKey(p.key)
		}
		switch p.idx {
		case 0:
			if p.typ != paramInt {
				return errors.New("Missing operator")
			}
			oper = int(p.num)
		case 1:
			n, err := c.number(p)
			if err != nil {
				return err
			}
			right = n
		default:
			return errTooManyPositional
		}
	}
	if err := c.needTransfer("calculate"); err != nil {
		return err
	}
	// Binary input is a big-endian unsigned number for modulo only.
	if c.st.binary && oper == operModulo {
		if right <= 0 {
			return errors.New("Binary modulo must be positive integer")
		}
		var tmp uint64
		data := c.st.transfer
		for _, b := range data[len(data)-min(len(data), 8):] {
			tmp = tmp<<8 | uint64(b)
		}
		c.st.setString(strconv.FormatUint(tmp%uint64(right), 10))
		return nil
	}
	value, err := strconv.ParseInt(string(c.st.transfer), 10, 64)
	if err != nil {
		return errors.New("Input is not a number")
	}
	switch oper {
	case operPlus:
		value += right
	case operMinus:
		value -= right
	case operStar:
		value *= right
	case operSlash:
		if right <= 0 {
			return errors.New("Division must be by positive integer")
		}
		value /= right
	case operModulo:
		if right <= 0 {
			return errors.New("Modulo must be by positive integer")
		}
		value %= right
	default:
		return errors.New("Missing operator")
	}
	c.st.unset()
	c.st.setData([]byte(strconv.FormatInt(value, 10)))
	return nil
}

func fnConcat(c *call) error {
	if err := c.needParams(); err != nil {
		return err
	}
	var b bytes.Buffer
	if c.st.set {
		b.Write(c.st.transfer)
	}
	for i := range c.params() {
		p := &c.params()[i]
		if p.key != "" {
			return errUnsupportedKey(p.key)
		}
		v, err := c.any(p)
		if err != nil {
			return err
		}
		b.WriteString(v)
	}
	c.st.setData(b.Bytes())
	return nil
}

func hashAs(algo string) filterFunc {
	return func(c *call) error { return hashWith(c, algo, false) }
}

func fnHash(c *call) error {
	if err := c.needParams(); err != nil {
		return err
	}
	p := &c.params()[0]
	if p.idx == -1 {
		return errors.New("No algorithm as first parameter")
	}
	algo, err := c.str(p)
	if err != nil {
		return err
	}
	return hashWith(c, algo, true)
}

func hashWith(c *call, algo string, algoFromParam bool) error {
	method, ok := hashMethods[algo]
	if !ok {
		return fmt.Errorf("Unsupported algorithm '%s'", algo)
	}
	rounds := int64(1)
	salt := ""
	for i := range c.params() {
		p := &c.params()[i]
		if p.key == "" {
			if !algoFromParam || p.idx > 0 {
				return errTooManyPositional
			}
			continue
		}
		switch p.key {
		case "rounds":
			n, err := c.number(p)
			if err != nil {
				return err
			}
			rounds = n
		case "salt":
			s, err := c.str(p)
			if err != nil {
				return err
			}
			salt = s
		default:
			return errUnsupportedKey(p.key)
		}
	}
	action := algo
	if algoFromParam {
		action = "hash"
	}
	if err := c.needTransfer(action); err != nil {
		return err
	}
	input := append([]byte(nil), c.st.transfer...)
	for range rounds {
		h := method()
		h.Write([]byte(salt))
		h.Write(input)
		input = h.Sum(nil)
	}
	c.st.setBinary(input)
	return nil
}

func fnBase64(c *call) error {
	enc := base64.StdEncoding
	url, pad := false, true
	for i := range c.params() {
		p := &c.params()[i]
		if p.key == "" {
			return errTooManyPositional
		}
		v, err := c.boolean(p)
		if err != nil {
			return err
		}
		switch p.key {
		case "pad":
			pad = v
		case "url":
			url = v
		default:
			return errUnsupportedKey(p.key)
		}
	}
	if url {
		enc = base64.URLEncoding
	}
	if !pad {
		enc = enc.WithPadding(base64.NoPadding)
	}
	if err := c.needTransfer("base64"); err != nil {
		return err
	}
	c.st.setString(enc.EncodeToString(c.st.transfer))
	return nil
}

func fnUnbase64(c *call) error {
	url, pad := false, true
	for i := range c.params() {
		p := &c.params()[i]
		if p.key == "" {
			return errTooManyPositional
		}
		n, err := c.number(p)
		if err != nil {
			return err
		}
		switch p.key {
		case "pad":
			if n != 0 && n != 1 {
				return errors.New("Supported values for pad are 0 or 1")
			}
			pad = n == 1
		case "url":
			if n != 0 && n != 1 {
				return errors.New("Supported values for url are 0 or 1")
			}
			url = n == 1
		default:
			return errUnsupportedKey(p.key)
		}
	}
	if err := c.needTransfer("unbase64"); err != nil {
		return err
	}
	enc := base64.StdEncoding
	if url {
		enc = base64.URLEncoding
	}
	in := strings.TrimRight(string(c.st.transfer), "=")
	_ = pad // padding is optional on input either way
	out, err := enc.WithPadding(base64.NoPadding).DecodeString(in)
	if err != nil {
		var ce base64.CorruptInputError
		if errors.As(err, &ce) {
			out, _ = enc.WithPadding(base64.NoPadding).DecodeString(in[:int(ce)])
		}
	}
	c.st.setBinary(out)
	return nil
}

func fnHex(c *call) error {
	var width int64
	for i := range c.params() {
		p := &c.params()[i]
		if p.key != "" {
			return errUnsupportedKey(p.key)
		}
		if p.idx != 0 {
			return errTooManyPositional
		}
		n, err := c.number(p)
		if err != nil {
			return err
		}
		width = n
	}
	if err := c.needTransfer("hex"); err != nil {
		return err
	}
	number, err := strconv.ParseUint(string(c.st.transfer), 10, 64)
	if err != nil {
		return errors.New("Input is not a number")
	}
	s := strconv.FormatUint(number, 16)
	if width < -maxPadding || width > maxPadding {
		return errors.New("Excessive padding")
	}
	switch {
	case width < 0:
		w := int(-width)
		for len(s) < w {
			s += "0"
		}
		s = s[:w]
	case width > 0:
		w := int(width)
		for len(s) < w {
			s = "0" + s
		}
		s = s[len(s)-w:]
	}
	c.st.setData([]byte(s))
	return nil
}

func fnUnhex(c *call) error {
	if err := c.noParams(); err != nil {
		return err
	}
	if err := c.needTransfer("unhex"); err != nil {
		return err
	}
	number, err := strconv.ParseUint(string(c.st.transfer), 16, 64)
	if err != nil {
		return errors.New("Input is not a hex number")
	}
	c.st.setString(strconv.FormatUint(number, 10))
	return nil
}

func fnHexlify(c *call) error {
	var width int64
	if ps := c.params(); len(ps) > 0 {
		if ps[0].key != "" {
			return errUnsupportedKey(ps[0].key)
		}
		n, err := c.number(&ps[0])
		if err != nil {
			return err
		}
		if n < 0 {
			return errors.New("Width must be positive")
		}
		width = n
	}
	if err := c.needTransfer("hexlify"); err != nil {
		return err
	}
	result := hex.EncodeToString(c.st.transfer)
	switch w := int(width); {
	case width == 0:
	case len(result) < w:
		if w-len(result) > maxPadding {
			return errors.New("Excessive padding")
		}
		result = strings.Repeat("0", w-len(result)) + result
	case len(result) > w:
		result = result[:w]
	}
	c.st.setString(result)
	return nil
}

func fnUnhexlify(c *call) error {
	if err := c.noParams(); err != nil {
		return err
	}
	if err := c.needTransfer("unhexlify"); err != nil {
		return err
	}
	if len(c.st.transfer)%2 != 0 {
		return errors.New("Not a hex value")
	}
	out, err := hex.DecodeString(string(c.st.transfer))
	if err != nil {
		return errors.New("Not a hex value")
	}
	c.st.setBinary(out)
	return nil
}

func fnReverse(c *call) error {
	if err := c.noParams(); err != nil {
		return err
	}
	if err := c.needTransfer("reverse"); err != nil {
		return err
	}
	in := c.st.transfer
	out := make([]byte, len(in))
	for i := range in {
		out[i] = in[len(in)-1-i]
	}
	c.st.setData(out)
	return nil
}

func fnTruncate(c *call) error {
	if err := c.needParams(); err != nil {
		return err
	}
	length, bits := int64(-1), false
	for i := range c.params() {
		p := &c.params()[i]
		switch {
		case p.key == "bits" || p.key == "" && p.idx == 0:
			n, err := c.number(p)
			if err != nil {
				return err
			}
			if n < 0 || n > math.MaxInt32 {
				return errors.New("Value of out of bounds")
			}
			length, bits = n, p.key == "bits"
		case p.key != "":
			return errUnsupportedKey(p.key)
		default:
			return errTooManyPositional
		}
	}
	if length < 0 {
		return errors.New("Missing truncation length")
	}
	if err := c.needTransfer("truncate"); err != nil {
		return err
	}
	out := append([]byte(nil), c.st.transfer...)
	if bits {
		out = truncateRshiftBits(out, uint(length))
	} else if int(length) < len(out) {
		out = out[:length]
	}
	c.st.setData(out)
	return nil
}

// truncateRshiftBits keeps the top bits of b, shifted to the right edge.
func truncateRshiftBits(b []byte, bits uint) []byte {
	if bits >= uint(len(b))*8 {
		return b
	}
	n := (bits + 7) / 8
	out := append([]byte(nil), b[:n]...)
	if shift := n*8 - bits; shift > 0 {
		var carry byte
		for i := range out {
			cur := out[i]
			out[i] = cur>>shift | carry
			carry = cur << (8 - shift)
		}
	}
	return out
}

func fnSubstr(c *call) error {
	if err := c.needParams(); err != nil {
		return err
	}
	used := int64(len(c.st.transfer))
	off, length := int64(0), used
	gotOff, gotLen := false, false
	for i := range c.params() {
		p := &c.params()[i]
		if p.key != "" {
			return errUnsupportedKey(p.key)
		}
		n, err := c.number(p)
		if err != nil {
			return err
		}
		switch p.idx {
		case 0:
			off, gotOff = n, true
		case 1:
			length, gotLen = n, true
		default:
			return errTooManyPositional
		}
	}
	if !gotOff {
		return errors.New("Missing offset parameter")
	}
	if err := c.needTransfer("substring"); err != nil {
		return err
	}
	if off < -used || off > used {
		return errors.New("Offset out of bounds")
	}
	if length < -used || length > used {
		return errors.New("Length out of bounds")
	}
	if length == 0 {
		c.st.setData(nil)
		return nil
	}
	if off < 0 {
		off += used
	}
	switch {
	case !gotLen:
		length = used - off
	case length < 0:
		length += used
		if length < off {
			return errors.New("Length out of bounds")
		}
		length -= off
	}
	if off < 0 || off > used {
		return errors.New("Offset out of bounds")
	}
	if length < 0 || length+off > used {
		return errors.New("Length out of bounds")
	}
	c.st.setData(append([]byte(nil), c.st.transfer[off:off+length]...))
	return nil
}

func fnLdapDN(c *call) error {
	if err := c.noParams(); err != nil {
		return err
	}
	if err := c.needTransfer("convert to ldap_dn"); err != nil {
		return err
	}
	c.st.setData([]byte(strings.ReplaceAll(string(c.st.transfer), ".", ",dc=")))
	return nil
}

// fixReplacement turns \N into $N and \\ into \, as the reference does before
// handing the replacement to its regex engine.
func fixReplacement(rep string) string {
	var b strings.Builder
	for i := 0; i < len(rep); i++ {
		if rep[i] != '\\' || i+1 >= len(rep) {
			b.WriteByte(rep[i])
			continue
		}
		switch n := rep[i+1]; {
		case n >= '0' && n <= '9':
			b.WriteByte('$')
			b.WriteByte(n)
			i++
		case n == '\\':
			b.WriteByte('\\')
			i++
		default:
			b.WriteByte('\\')
		}
	}
	return b.String()
}

func fnRegexp(c *call) error {
	if err := c.needParams(); err != nil {
		return err
	}
	var pat, rep *string
	for i := range c.params() {
		p := &c.params()[i]
		if p.key != "" {
			return errUnsupportedKey(p.key)
		}
		v, err := c.str(p)
		if err != nil {
			return err
		}
		switch p.idx {
		case 0:
			pat = &v
		case 1:
			rep = &v
		default:
			return errTooManyPositional
		}
	}
	if pat == nil {
		return errors.New("Missing pattern and replacement parameters")
	}
	if rep == nil {
		return errors.New("Missing replacement parameter")
	}
	if err := c.needTransfer("regexp"); err != nil {
		return err
	}
	re, err := regexp.Compile(*pat)
	if err != nil {
		return err
	}
	r := *rep
	if strings.Contains(r, `\`) {
		r = fixReplacement(r)
	}
	in := string(c.st.transfer)
	m := firstNonEmptyMatch(re, in)
	if m == nil {
		return nil
	}
	out := re.ExpandString(nil, replacementTemplate(r), in, m)
	c.st.setData([]byte(in[:m[0]] + string(out) + in[m[1]:]))
	return nil
}

// replacementTemplate spells $N as ${N}: Go reads $1x as a group named 1x,
// the replacement syntax this mirrors as group 1 and an x.
func replacementTemplate(r string) string {
	var b strings.Builder
	for i := 0; i < len(r); i++ {
		if r[i] != '$' || i+1 >= len(r) || r[i+1] < '0' || r[i+1] > '9' {
			b.WriteByte(r[i])
			if r[i] == '$' && i+1 < len(r) && r[i+1] == '$' {
				b.WriteByte('$')
				i++
			}
			continue
		}
		j := i + 1
		for j < len(r) && r[j] >= '0' && r[j] <= '9' {
			j++
		}
		b.WriteString("${" + r[i+1:j] + "}")
		i = j - 1
	}
	return b.String()
}

// firstNonEmptyMatch is the leftmost match that is not empty, as a search with
// empty matches refused finds it.
func firstNonEmptyMatch(re *regexp.Regexp, s string) []int {
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		if m[1] > m[0] {
			return m
		}
	}
	return nil
}

func numberAs(order binary.ByteOrder) filterFunc {
	return func(c *call) error {
		if err := c.noParams(); err != nil {
			return err
		}
		if err := c.needTransfer("convert to number"); err != nil {
			return err
		}
		d := c.st.transfer
		var v uint64
		switch len(d) {
		case 1:
			v = uint64(d[0])
		case 2:
			v = uint64(order.Uint16(d))
		case 4:
			v = uint64(order.Uint32(d))
		case 8:
			v = order.Uint64(d)
		default:
			return fmt.Errorf("Cannot convert '%d' bytes to number", len(d))
		}
		c.st.setString(strconv.FormatUint(v, 10))
		return nil
	}
}

func indexOf(c *call, idx int, sep string) error {
	if sep == "" {
		return errors.New("Empty separator")
	}
	tokens := strings.Split(string(c.st.transfer), sep)
	if idx < 0 {
		idx += len(tokens)
	}
	if idx < 0 || idx >= len(tokens) {
		return errors.New("Position out of bounds")
	}
	c.st.setString(tokens[idx])
	return nil
}

func fnIndex(c *call) error {
	if err := c.needParams(); err != nil {
		return err
	}
	var sep *string
	idx, gotIdx := 0, false
	for i := range c.params() {
		p := &c.params()[i]
		if p.key != "" {
			return errUnsupportedKey(p.key)
		}
		switch p.idx {
		case 0:
			v, err := c.str(p)
			if err != nil {
				return err
			}
			sep = &v
		case 1:
			n, err := c.number(p)
			if err != nil {
				return err
			}
			if n < math.MinInt32 || n > math.MaxInt32 {
				return errors.New("Position out of bounds")
			}
			idx, gotIdx = int(n), true
		default:
			return errTooManyPositional
		}
	}
	if sep == nil {
		return errors.New("Missing separator and index parameters")
	}
	if !gotIdx {
		return errors.New("Missing index parameter")
	}
	if err := c.needTransfer("index"); err != nil {
		return err
	}
	return indexOf(c, idx, *sep)
}

func fnUsername(c *call) error {
	if err := c.noParams(); err != nil {
		return err
	}
	if err := c.needTransfer("get username from"); err != nil {
		return err
	}
	return indexOf(c, 0, "@")
}

// fnDomain is everything after the first @, @ included in what follows.
func fnDomain(c *call) error {
	if err := c.noParams(); err != nil {
		return err
	}
	if err := c.needTransfer("get domain from"); err != nil {
		return err
	}
	s := string(c.st.transfer)
	_, after, _ := strings.Cut(s, "@")
	c.st.setString(after)
	return nil
}

func fnList(c *call) error {
	sep := ","
	for i := range c.params() {
		p := &c.params()[i]
		if p.key != "" {
			return errUnsupportedKey(p.key)
		}
		if p.idx > 0 {
			return errTooManyPositional
		}
		v, err := c.str(&c.params()[0])
		if err != nil {
			return err
		}
		sep = v
	}
	if err := c.needTransfer("generate list from"); err != nil {
		return err
	}
	parts := strings.Split(string(c.st.transfer), "\t")
	for i, part := range parts {
		parts[i] = tabUnescape(part)
	}
	c.st.setString(strings.Join(parts, sep))
	return nil
}

// tabUnescape reverses the \x01 escape of tab-separated values.
func tabUnescape(s string) string {
	if !strings.Contains(s, "\x01") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 1 || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case '1':
			b.WriteByte(1)
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		case '0':
			b.WriteByte(0)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func fillAs(left bool) filterFunc {
	return func(c *call) error {
		if err := c.needParams(); err != nil {
			return err
		}
		amount, filler := 0, "0"
		for i := range c.params() {
			p := &c.params()[i]
			if p.key != "" {
				return errUnsupportedKey(p.key)
			}
			switch p.idx {
			case 0:
				n, err := c.number(p)
				if err != nil {
					return err
				}
				if n < 1 || n > math.MaxInt32 {
					return errors.New("Fill length out of bounds")
				}
				amount = int(n)
			case 1:
				v, err := c.str(p)
				if err != nil {
					return err
				}
				filler = v
			default:
				return errTooManyPositional
			}
		}
		if amount < 1 {
			return errors.New("Missing amount")
		}
		if err := c.needTransfer("fill"); err != nil {
			return err
		}
		s := string(c.st.transfer)
		for len(s) < amount && filler != "" {
			if left {
				s = filler + s
			} else {
				s += filler
			}
		}
		c.st.setData([]byte(s))
		return nil
	}
}

// fnText makes the value printable UTF-8: invalid bytes and control
// characters become U+FFFD.
func fnText(c *call) error {
	if err := c.noParams(); err != nil {
		return err
	}
	if err := c.needTransfer("text"); err != nil {
		return err
	}
	in := c.st.transfer
	if i := bytes.IndexByte(in, 0); i >= 0 {
		in = in[:i]
	}
	var b strings.Builder
	for len(in) > 0 {
		r, n := utf8.DecodeRune(in)
		switch {
		case r == utf8.RuneError && n <= 1:
			b.WriteRune(utf8.RuneError)
			n = 1
		case in[0] < 0x20 || in[0] == 0x7f:
			b.WriteRune(utf8.RuneError)
		default:
			b.Write(in[:n])
		}
		in = in[n:]
	}
	c.st.setString(b.String())
	return nil
}

func fnEscape(c *call) error {
	if err := c.noParams(); err != nil {
		return err
	}
	if err := c.needTransfer("escape"); err != nil {
		return err
	}
	if c.st.params.Escape == nil {
		return errors.New("No escape function available")
	}
	v, err := c.st.params.Escape(string(c.st.transfer))
	if err != nil {
		return err
	}
	c.st.setString(v)
	return nil
}

func fnSafe(c *call) error {
	if c.more {
		return errors.New("safe filter must be last in the filter chain")
	}
	c.st.safe = true
	return nil
}
