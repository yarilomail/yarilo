package config

import (
	"strings"
	"testing"
)

// A mode that is not octal is refused at load, naming the key, rather than
// becoming a socket nobody can open (#2415).
func TestASocketModeThatIsNotOctalIsRefused(t *testing.T) {
	if err := Defaults().validate(); err != nil {
		t.Fatalf("defaults refused: %v", err)
	}
	for key, set := range map[string]func(*Config){
		"auth_service.socket_mode":        func(c *Config) { c.AuthService.SocketMode = "0668" },
		"auth_service.master_socket_mode": func(c *Config) { c.AuthService.MasterSocketMode = "rw" },
		"warden_service.socket_mode":      func(c *Config) { c.WardenService.SocketMode = "1777" },
		"locks_service.socket_mode":       func(c *Config) { c.LocksService.SocketMode = "" },
	} {
		c := Defaults()
		set(c)
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v, want a refusal naming the key", key, err)
		}
	}
}

func TestSocketPermissionsComeFromTheirKeys(t *testing.T) {
	c := Defaults()
	c.AuthService.SocketGroup = "mail"
	c.AuthService.MasterSocketMode = "0600"
	c.WardenService.SocketMode = "0640"
	c.LocksService.SocketGroup = "yarilo"
	for _, tc := range []struct {
		name  string
		mode  uint32
		group string
		got   func() (uint32, string)
	}{
		{"auth", 0o660, "mail", func() (uint32, string) { p := c.AuthService.Socket(); return uint32(p.Mode), p.Group }},
		{"auth master", 0o600, "", func() (uint32, string) { p := c.AuthService.MasterSocket(); return uint32(p.Mode), p.Group }},
		{"warden", 0o640, "", func() (uint32, string) { p := c.WardenService.Socket(); return uint32(p.Mode), p.Group }},
		{"locks", 0o660, "yarilo", func() (uint32, string) { p := c.LocksService.SocketPerm(); return uint32(p.Mode), p.Group }},
	} {
		if mode, group := tc.got(); mode != tc.mode || group != tc.group {
			t.Errorf("%s: %o %q, want %o %q", tc.name, mode, group, tc.mode, tc.group)
		}
	}
}
