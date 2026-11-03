// Package userfmt expands the username templates yarilo-auth and OAuth2 share.
package userfmt

import (
	"fmt"
	"strings"
)

// Format expands %u, %{user}, %n and %d, each with an optional L to lowercase;
// an unknown substitution is an error, so a typo fails at startup.
func Format(template, user string) (string, error) {
	if template == "" || template == "%{user}" || template == "%u" {
		return user, nil
	}
	var b strings.Builder
	b.Grow(len(template))
	for i := 0; i < len(template); i++ {
		c := template[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(template) {
			return "", fmt.Errorf("userfmt: trailing %% in template %q", template)
		}
		i++
		// %{name} long form.
		if template[i] == '{' {
			end := strings.IndexByte(template[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("userfmt: unterminated %%{ in template %q", template)
			}
			name := template[i+1 : i+end]
			i += end
			switch name {
			case "user":
				b.WriteString(user)
			default:
				return "", fmt.Errorf("userfmt: unknown %%{%s} in template %q", name, template)
			}
			continue
		}
		// %L… lowercase prefix.
		lower := false
		if template[i] == 'L' {
			lower = true
			if i+1 >= len(template) {
				return "", fmt.Errorf("userfmt: trailing %%L in template %q", template)
			}
			i++
		}
		switch template[i] {
		case 'u':
			if lower {
				b.WriteString(strings.ToLower(user))
			} else {
				b.WriteString(user)
			}
		case 'n':
			lp := localPart(user)
			if lower {
				lp = strings.ToLower(lp)
			}
			b.WriteString(lp)
		case 'd':
			dom := domainPart(user)
			if lower {
				dom = strings.ToLower(dom)
			}
			b.WriteString(dom)
		default:
			return "", fmt.Errorf("userfmt: unknown %%%c in template %q", template[i], template)
		}
	}
	return b.String(), nil
}

// localPart returns the segment before the first '@' in user,
// or the whole string when no '@' is present.
func localPart(user string) string {
	if at := strings.IndexByte(user, '@'); at >= 0 {
		return user[:at]
	}
	return user
}

// domainPart returns the segment after the first '@' in user, or
// an empty string when no '@' is present.
func domainPart(user string) string {
	if at := strings.IndexByte(user, '@'); at >= 0 {
		return user[at+1:]
	}
	return ""
}
