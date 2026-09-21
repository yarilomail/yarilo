package imaptext

import "strings"

// addrItem is one entry of an address list: an address, or the marker that
// opens or closes a group. The reference writes a group as a pair of markers
// around its members (imap-envelope.c:18-44, message-address.c).
type addrItem struct {
	name    string
	mailbox string
	host    string
	// group is set on the two markers: the opener carries the group's name in
	// mailbox, the closer carries nothing.
	group bool
}

// parseAddressList splits a raw header value into address structures without
// decoding anything: an encoded word stays encoded, because that is what the
// reference caches and what a client is shown.
func parseAddressList(value string) []addrItem {
	var out []addrItem
	for _, item := range splitTop(value, ',', true) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if name, members, ok := splitGroup(item); ok {
			out = append(out, addrItem{mailbox: name, group: true})
			for _, member := range splitTop(members, ',', true) {
				if member = strings.TrimSpace(member); member != "" {
					out = append(out, parseAddress(member))
				}
			}
			out = append(out, addrItem{group: true})
			continue
		}
		out = append(out, parseAddress(item))
	}
	return out
}

// splitGroup recognises "name: members;" -- a colon at the top level that no
// angle-addr swallowed.
func splitGroup(item string) (name, members string, ok bool) {
	depth, quoted := 0, false
	for i := 0; i < len(item); i++ {
		switch c := item[i]; {
		case quoted:
			if c == '\\' {
				i++
			} else if c == '"' {
				quoted = false
			}
		case c == '"':
			quoted = true
		case c == '<' || c == '(':
			depth++
		case c == '>' || c == ')':
			depth--
		case c == ':' && depth == 0:
			rest := item[i+1:]
			return strings.TrimSpace(item[:i]), strings.TrimSuffix(strings.TrimSpace(rest), ";"), true
		}
	}
	return "", "", false
}

// parseAddress reads one address: a display name and an angle-addr, or a bare
// addr-spec. Quoting is resolved, encoded words are not.
func parseAddress(item string) addrItem {
	var out addrItem
	if i := strings.LastIndexByte(item, '<'); i >= 0 {
		if j := strings.IndexByte(item[i:], '>'); j >= 0 {
			out.name = unquote(strings.TrimSpace(item[:i]))
			item = item[i+1 : i+j]
		}
	}
	local, domain := splitAddrSpec(strings.TrimSpace(item))
	out.mailbox, out.host = unquote(local), domain
	return out
}

// splitAddrSpec cuts at the last unquoted "@": a quoted local part may hold
// one of its own.
func splitAddrSpec(spec string) (local, domain string) {
	quoted := false
	at := -1
	for i := 0; i < len(spec); i++ {
		switch c := spec[i]; {
		case quoted:
			if c == '\\' {
				i++
			} else if c == '"' {
				quoted = false
			}
		case c == '"':
			quoted = true
		case c == '@':
			at = i
		}
	}
	if at < 0 {
		return spec, ""
	}
	return spec[:at], spec[at+1:]
}

// splitTop splits on sep at the top level, outside quotes, angle brackets and
// comments. Groups keep their semicolon, so a comma inside one is not a split.
func splitTop(s string, sep byte, keepGroups bool) []string {
	var out []string
	depth, group, quoted, start := 0, false, false, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quoted:
			if c == '\\' {
				i++
			} else if c == '"' {
				quoted = false
			}
		case c == '"':
			quoted = true
		case c == '<' || c == '(':
			depth++
		case c == '>' || c == ')':
			if depth > 0 {
				depth--
			}
		case c == ':' && keepGroups && depth == 0:
			group = true
		case c == ';' && group:
			group = false
		case c == sep && depth == 0 && !group:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// unquote resolves a quoted string: the reference's parser hands the writer the
// content, not the quoting the header used.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	var b strings.Builder
	for i := 1; i < len(s)-1; i++ {
		if s[i] == '\\' && i+1 < len(s)-1 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
