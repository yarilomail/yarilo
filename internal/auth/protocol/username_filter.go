package protocol

import "strings"

// UsernameFilter is a passdb entry's username_filter as the reference reads it:
// masks on spaces and commas, * and ?, case-sensitive, ! excludes; empty takes all.
type UsernameFilter []string

// ParseUsernameFilter splits a username_filter setting into its masks.
func ParseUsernameFilter(s string) UsernameFilter {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' })
}

// Accepts reports whether username passes: an exclusion that matches refuses,
// and when there are inclusions one of them must match.
func (f UsernameFilter) Accepts(username string) bool {
	haveIncludes, matched := false, false
	for _, mask := range f {
		exclude := strings.HasPrefix(mask, "!")
		if !exclude {
			haveIncludes = true
		}
		if wildcardMatch(username, strings.TrimPrefix(mask, "!")) {
			if exclude {
				return false
			}
			matched = true
		}
	}
	return matched || !haveIncludes
}

// wildcardMatch matches data against mask: * is any run, ? one byte.
func wildcardMatch(data, mask string) bool {
	d, m := 0, 0
	star, mark := -1, 0
	for d < len(data) {
		switch {
		case m < len(mask) && (mask[m] == '?' || mask[m] == data[d]):
			d++
			m++
		case m < len(mask) && mask[m] == '*':
			star, mark = m, d
			m++
		case star >= 0:
			m = star + 1
			mark++
			d = mark
		default:
			return false
		}
	}
	for m < len(mask) && mask[m] == '*' {
		m++
	}
	return m == len(mask)
}

// CredentialsLookup is the reference's lookup_credentials: whether a passdb
// knows the user, no password asked.
type CredentialsLookup interface {
	LookupCredentials(username string) (bool, error)
}

// UserExists asks each passdb that can look a user up without a password;
// the first that knows the user decides, an error stops the walk.
func UserExists(passdbs []Passdb, username string) (bool, error) {
	for _, db := range passdbs {
		cl, ok := db.(CredentialsLookup)
		if !ok {
			continue
		}
		found, err := cl.LookupCredentials(username)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}
