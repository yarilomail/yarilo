package config

import (
	"strings"
	"testing"
)

func everyEntry() *Config {
	c := &Config{}
	for _, e := range c.Services.Entries() {
		*e.Svc = &ServiceConfig{Enabled: true}
	}
	return c
}

func activeEntries(c *Config) []string {
	var on []string
	for _, e := range c.Services.Entries() {
		if (*e.Svc).Active() {
			on = append(on, e.Name)
		}
	}
	return on
}

// A session backend keeps the one entry named after it and drops the rest:
// the login proxy's entries, with the certificate, are not its own (#1863, #2415).
func TestASessionBinaryKeepsOnlyItsOwnEntry(t *testing.T) {
	for _, role := range []SessionRole{RoleIMAP, RolePOP3, RoleLMTP, RoleManageSieve, RoleSubmission, RoleJMAP} {
		cfg := everyEntry()
		if err := KeepOnlySessionListener(cfg, role); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(activeEntries(cfg), ","); got != string(role) {
			t.Errorf("%s keeps %q, want only its own entry", role, got)
		}
	}
}

// Without its own entry a backend does not start, and says which key it lacks;
// it never takes a neighbour's port (#2415).
func TestASessionBinaryWithoutItsEntryRefusesToStart(t *testing.T) {
	cfg := everyEntry()
	cfg.Services.IMAP = nil
	err := KeepOnlySessionListener(cfg, RoleIMAP)
	if err == nil || !strings.Contains(err.Error(), "services.imap missing") {
		t.Fatalf("err = %v, want a refusal naming services.imap", err)
	}
}

// The single binary keeps the backend entries and none of the login proxies'.
func TestTheSingleBinaryKeepsTheBackendEntries(t *testing.T) {
	cfg := everyEntry()
	if err := KeepBackendListeners(cfg); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(activeEntries(cfg), ","); got != "imap,pop3,submission,lmtp,managesieve,jmap" {
		t.Errorf("the single binary keeps %q", got)
	}
	cfg = everyEntry()
	for _, e := range cfg.Services.Entries() {
		if !strings.HasSuffix(e.Name, "_login") {
			*e.Svc = nil
		}
	}
	if err := KeepBackendListeners(cfg); err == nil {
		t.Error("the single binary started with login entries only")
	}
}

func TestRequireListenersNamesEveryMissingKey(t *testing.T) {
	cfg := everyEntry()
	cfg.Services.IMAPLogin, cfg.Services.IMAPSLogin = nil, nil
	err := RequireListeners(cfg, "imap_login", "imaps_login", "imap")
	if err == nil || !strings.Contains(err.Error(), "services.imap_login, services.imaps_login missing") {
		t.Fatalf("err = %v, want both login keys named", err)
	}
	if err := RequireListeners(cfg, "imap"); err != nil {
		t.Errorf("a present key refused: %v", err)
	}
}
