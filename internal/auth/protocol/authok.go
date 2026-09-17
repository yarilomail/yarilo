package protocol

import "strings"

// authOKField is one field of an OK answer. Writing and reading go through the
// same list so a field cannot be put on the wire that no reader takes off it.
type authOKField struct {
	key string
	get func(*AuthResponse) string
	set func(*AuthResponse, string)
}

var authOKFields = []authOKField{
	{"home", func(r *AuthResponse) string { return r.Home }, func(r *AuthResponse, v string) { r.Home = v }},
	{"mail", func(r *AuthResponse) string { return r.MailLoc }, func(r *AuthResponse, v string) { r.MailLoc = v }},
	{"mailbox_format", func(r *AuthResponse) string { return r.MailboxFormat }, func(r *AuthResponse, v string) { r.MailboxFormat = v }},
	{"groups", func(r *AuthResponse) string { return strings.Join(r.Groups, ",") }, func(r *AuthResponse, v string) { r.Groups = SplitCSV(v) }},
	{"acl_user", func(r *AuthResponse) string { return r.ACLUser }, func(r *AuthResponse, v string) { r.ACLUser = v }},
	{"acl_groups", func(r *AuthResponse) string { return strings.Join(r.ACLGroups, ",") }, func(r *AuthResponse, v string) { r.ACLGroups = SplitCSV(v) }},
	{"quota_rule", func(r *AuthResponse) string { return strings.Join(r.QuotaRules, ",") }, func(r *AuthResponse, v string) { r.QuotaRules = SplitCSV(v) }},
	{"quota_over_flag", func(r *AuthResponse) string { return r.QuotaOverFlag }, func(r *AuthResponse, v string) { r.QuotaOverFlag = v }},
	{"director_tag", func(r *AuthResponse) string { return r.DirectorTag }, func(r *AuthResponse, v string) { r.DirectorTag = v }},
	{"volatile_dir", func(r *AuthResponse) string { return r.VolatileDir }, func(r *AuthResponse, v string) { r.VolatileDir = v }},
	{"index_dir", func(r *AuthResponse) string { return r.IndexDir }, func(r *AuthResponse, v string) { r.IndexDir = v }},
	{"control_dir", func(r *AuthResponse) string { return r.ControlDir }, func(r *AuthResponse, v string) { r.ControlDir = v }},
	{"alt_dir", func(r *AuthResponse) string { return r.AltDir }, func(r *AuthResponse, v string) { r.AltDir = v }},
	{"mail_path", func(r *AuthResponse) string { return r.MailPath }, func(r *AuthResponse, v string) { r.MailPath = v }},
	{"inbox_path", func(r *AuthResponse) string { return r.InboxPath }, func(r *AuthResponse, v string) { r.InboxPath = v }},
}

// AuthOKTokens renders the userdb half of an OK answer.
func AuthOKTokens(res *AuthResponse) []string {
	out := make([]string, 0, len(authOKFields))
	for _, f := range authOKFields {
		if v := f.get(res); v != "" {
			out = append(out, f.key+"="+v)
		}
	}
	return out
}

// ApplyAuthOKToken fills res from one `key=value` of an OK answer, accepting the
// userdb_ prefix the chain emits for the same field. Reports whether it knew it.
func ApplyAuthOKToken(res *AuthResponse, tok string) bool {
	key, value, ok := strings.Cut(tok, "=")
	if !ok {
		return false
	}
	key = strings.TrimPrefix(key, "userdb_")
	for _, f := range authOKFields {
		if f.key == key {
			f.set(res, value)
			return true
		}
	}
	return false
}
