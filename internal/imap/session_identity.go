package imap

// username is the identity diagnostics name this session by; empty before login.
func (s *session) username() string {
	if s.userInfo == nil {
		return ""
	}
	return s.userInfo.Username
}
