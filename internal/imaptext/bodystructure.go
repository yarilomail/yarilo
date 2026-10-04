package imaptext

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
)

// defaultCharset is what the reference writes into a text part that names no
// charset.
const defaultCharset = "us-ascii"

// What the reference writes for a childless multipart, which the grammar does
// not allow to be empty.
const (
	emptyBody          = `("text" "plain" ("charset" "us-ascii") NIL NIL "7bit" 0 0)`
	emptyBodyStructure = `("text" "plain" ("charset" "us-ascii") NIL NIL "7bit" 0 0 NIL NIL NIL NIL)`
)

// maxBodyStructureDepth bounds recursion on the way in: a cached string is
// data, and a nested one from elsewhere must not cost the process its stack.
const maxBodyStructureDepth = 50

// WriteBodyStructure writes the structure as the reference does, unparenthesised;
// extended gives BODYSTRUCTURE, else BODY.
func WriteBodyStructure(bs imaplib.BodyStructure, extended bool) (string, bool) {
	var b strings.Builder
	if !writeBodyStructure(&b, bs, extended) {
		return "", false
	}
	return b.String(), true
}

// Canonical is the structure as the cache hands it back, so the first FETCH
// answers what later ones do (#2146); a kind the cache refuses comes back as is.
func Canonical(bs imaplib.BodyStructure) imaplib.BodyStructure {
	enc, ok := WriteBodyStructure(bs, true)
	if !ok {
		return bs
	}
	if out, ok := ParseBodyStructure(enc); ok {
		return out
	}
	return bs
}

// Parameters go in name order; an unknown kind is refused, never written as an
// empty text part -- a wrong answer from cache is worse than a miss.
func writeBodyStructure(b *strings.Builder, bs imaplib.BodyStructure, extended bool) bool {
	switch p := bs.(type) {
	case *imaplib.BodyStructureMultiPart:
		return writeMultiPart(b, p, extended)
	case *imaplib.BodyStructureSinglePart:
		return writeSinglePart(b, p, extended)
	default:
		return false
	}
}

func writeMultiPart(b *strings.Builder, p *imaplib.BodyStructureMultiPart, extended bool) bool {
	if len(p.Children) == 0 {
		if extended {
			b.WriteString(emptyBodyStructure)
		} else {
			b.WriteString(emptyBody)
		}
	}
	for _, child := range p.Children {
		b.WriteByte('(')
		if !writeBodyStructure(b, child, extended) {
			return false
		}
		b.WriteByte(')')
	}
	b.WriteByte(' ')
	appendString(b, p.Subtype)
	if !extended {
		return true
	}
	b.WriteByte(' ')
	var params map[string]string
	var order []string
	var disp *imaplib.BodyStructureDisposition
	var lang []string
	var loc string
	if p.Extended != nil {
		params, order, disp, lang, loc = p.Extended.Params, p.Extended.ParamOrder, p.Extended.Disposition, p.Extended.Language, p.Extended.Location
	}
	writeParams(b, params, order, false)
	writeCommon(b, disp, lang, loc)
	return true
}

func writeSinglePart(b *strings.Builder, p *imaplib.BodyStructureSinglePart, extended bool) bool {
	typ, sub := p.Type, p.Subtype
	if typ == "" {
		typ, sub = "text", "plain"
	}
	text := strings.EqualFold(typ, "text")
	appendString(b, typ)
	b.WriteByte(' ')
	appendString(b, sub)
	b.WriteByte(' ')
	writeParams(b, p.Params, p.ParamOrder, text)
	b.WriteByte(' ')
	AppendNString(b, p.ID, p.ID != "")
	b.WriteByte(' ')
	AppendNString(b, p.Description, p.Description != "")
	b.WriteByte(' ')
	if p.Encoding == "" {
		b.WriteString(`"7bit"`)
	} else {
		appendString(b, p.Encoding)
	}
	fmt.Fprintf(b, " %d", p.Size)

	switch {
	case text:
		lines := int64(0)
		if p.Text != nil {
			lines = p.Text.NumLines
		}
		fmt.Fprintf(b, " %d", lines)
	case p.MessageRFC822 != nil:
		b.WriteString(" (")
		b.WriteString(WriteEnvelope(p.MessageRFC822.Envelope))
		b.WriteString(") (")
		if !writeBodyStructure(b, p.MessageRFC822.BodyStructure, extended) {
			return false
		}
		b.WriteByte(')')
		fmt.Fprintf(b, " %d", p.MessageRFC822.NumLines)
	}
	if !extended {
		return true
	}
	// md5 is not kept: nothing computes it here, and NIL is what the
	// reference writes when it has none.
	b.WriteString(" NIL")
	var disp *imaplib.BodyStructureDisposition
	var lang []string
	var loc string
	if p.Extended != nil {
		disp, lang, loc = p.Extended.Disposition, p.Extended.Language, p.Extended.Location
	}
	writeCommon(b, disp, lang, loc)
	return true
}

// writeCommon is the disposition, language and location tail both part kinds
// carry (as the reference does).
func writeCommon(b *strings.Builder, disp *imaplib.BodyStructureDisposition, lang []string, loc string) {
	b.WriteByte(' ')
	if disp == nil {
		b.WriteString("NIL")
	} else {
		b.WriteByte('(')
		appendString(b, disp.Value)
		b.WriteByte(' ')
		writeParams(b, disp.Params, disp.ParamOrder, false)
		b.WriteByte(')')
	}
	b.WriteByte(' ')
	if len(lang) == 0 {
		b.WriteString("NIL")
	} else {
		b.WriteByte('(')
		for i, l := range lang {
			if i > 0 {
				b.WriteByte(' ')
			}
			appendString(b, l)
		}
		b.WriteByte(')')
	}
	b.WriteByte(' ')
	AppendNString(b, loc, loc != "")
}

// params_write: message order, a text part always names a charset (last when
// the message gave none), an absent list is NIL unless the default is owed.
func writeParams(b *strings.Builder, params map[string]string, order []string, defaultCharsetWanted bool) {
	if !defaultCharsetWanted && len(params) == 0 {
		b.WriteString("NIL")
		return
	}
	names, keys := orderedNames(params, order)

	b.WriteByte('(')
	seenCharset := false
	for i, name := range names {
		if i > 0 {
			b.WriteByte(' ')
		}
		if defaultCharsetWanted && strings.EqualFold(name, "charset") {
			seenCharset = true
		}
		appendString(b, name)
		b.WriteByte(' ')
		appendString(b, params[keys[i]])
	}
	if defaultCharsetWanted && !seenCharset {
		if len(names) > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(b, `"charset" %q`, defaultCharset)
	}
	b.WriteByte(')')
}

// ParseBodyStructure reads a cached body structure, ours or the reference's.
// A malformation is (nil, false): a miss, never an error.
func ParseBodyStructure(s string) (imaplib.BodyStructure, bool) {
	p := &parser{in: "(" + s + ")"}
	a, ok := p.arg()
	if !ok || !p.eof() || !a.list {
		return nil, false
	}
	return bodyStructureFromArg(a, 0)
}

func bodyStructureFromArg(a arg, depth int) (imaplib.BodyStructure, bool) {
	if !a.list || len(a.items) == 0 || depth > maxBodyStructureDepth {
		return nil, false
	}
	if a.items[0].list {
		return multiPartFromArg(a, depth)
	}
	return singlePartFromArg(a, depth)
}

func multiPartFromArg(a arg, depth int) (imaplib.BodyStructure, bool) {
	out := &imaplib.BodyStructureMultiPart{}
	i := 0
	for ; i < len(a.items) && a.items[i].list; i++ {
		child, ok := bodyStructureFromArg(a.items[i], depth+1)
		if !ok {
			return nil, false
		}
		out.Children = append(out.Children, child)
	}
	if i >= len(a.items) {
		return nil, false
	}
	out.Subtype = a.items[i].str
	i++
	if i >= len(a.items) {
		return out, true
	}
	ext := &imaplib.BodyStructureMultiPartExt{}
	ext.Params, ext.ParamOrder = paramsFromArg(a.items[i])
	i++
	ext.Disposition, ext.Language, ext.Location = commonFromArgs(a.items, i)
	out.Extended = ext
	return out, true
}

func singlePartFromArg(a arg, depth int) (imaplib.BodyStructure, bool) {
	if len(a.items) < 7 {
		return nil, false
	}
	out := &imaplib.BodyStructureSinglePart{
		Type:        a.items[0].str,
		Subtype:     a.items[1].str,
		ID:          a.items[3].str,
		Description: a.items[4].str,
		Encoding:    a.items[5].str,
	}
	out.Params, out.ParamOrder = paramsFromArg(a.items[2])
	size, err := strconv.ParseUint(a.items[6].str, 10, 32)
	if err != nil {
		return nil, false
	}
	out.Size = uint32(size)

	i := 7
	switch {
	case strings.EqualFold(out.Type, "text"):
		if i < len(a.items) {
			lines, lerr := strconv.ParseInt(a.items[i].str, 10, 64)
			if lerr != nil {
				return nil, false
			}
			out.Text = &imaplib.BodyStructureText{NumLines: lines}
			i++
		}
	case strings.EqualFold(out.Type, "message") && strings.EqualFold(out.Subtype, "rfc822"):
		if i+2 >= len(a.items) {
			return nil, false
		}
		if !a.items[i].list {
			return nil, false
		}
		env, ok := envelopeFromArgs(a.items[i].items)
		if !ok {
			return nil, false
		}
		child, ok := bodyStructureFromArg(a.items[i+1], depth+1)
		if !ok {
			return nil, false
		}
		lines, lerr := strconv.ParseInt(a.items[i+2].str, 10, 64)
		if lerr != nil {
			return nil, false
		}
		out.MessageRFC822 = &imaplib.BodyStructureMessageRFC822{Envelope: env, BodyStructure: child, NumLines: lines}
		i += 3
	}
	if i >= len(a.items) {
		return out, true
	}
	i++ // md5, which nothing here keeps
	ext := &imaplib.BodyStructureSinglePartExt{}
	ext.Disposition, ext.Language, ext.Location = commonFromArgs(a.items, i)
	out.Extended = ext
	return out, true
}

func commonFromArgs(items []arg, i int) (*imaplib.BodyStructureDisposition, []string, string) {
	var disp *imaplib.BodyStructureDisposition
	var lang []string
	var loc string
	if i < len(items) && items[i].list && len(items[i].items) >= 1 {
		disp = &imaplib.BodyStructureDisposition{Value: items[i].items[0].str}
		disp.Params, disp.ParamOrder = paramsFromArg(items[i].items[1])
	}
	i++
	if i < len(items) {
		switch {
		case items[i].list:
			for _, l := range items[i].items {
				lang = append(lang, l.str)
			}
		case items[i].present:
			lang = []string{items[i].str}
		}
	}
	i++
	if i < len(items) {
		loc = items[i].str
	}
	return disp, lang, loc
}

// paramsFromArg keeps the order the list gave, which is the message's.
func paramsFromArg(a arg) (map[string]string, []string) {
	if !a.list || len(a.items) < 2 {
		return nil, nil
	}
	out := make(map[string]string, len(a.items)/2)
	order := make([]string, 0, len(a.items)/2)
	for i := 0; i+1 < len(a.items); i += 2 {
		if _, dup := out[a.items[i].str]; !dup {
			order = append(order, a.items[i].str)
		}
		out[a.items[i].str] = a.items[i+1].str
	}
	return out, order
}

// orderedNames lists params in the given order and spelling (a name is found as
// spelled or lowercased), then any it leaves out, sorted; keys index params.
func orderedNames(params map[string]string, order []string) (names, keys []string) {
	seen := make(map[string]bool, len(params))
	for _, name := range order {
		key := name
		if _, ok := params[key]; !ok {
			key = strings.ToLower(name)
		}
		if _, ok := params[key]; ok && !seen[key] {
			seen[key] = true
			names = append(names, name)
			keys = append(keys, key)
		}
	}
	var rest []string
	for name := range params {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(names, rest...), append(keys, rest...)
}
