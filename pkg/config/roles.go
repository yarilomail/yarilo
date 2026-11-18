package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/yarilomail/yarilo/v2/internal/cluster/ring"
	"github.com/yarilomail/yarilo/v2/internal/fts/language"
	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/pkg/redisopt"
)

// roleChecks are the refusals a role's start makes from the config alone, no
// file, socket or peer involved; each binary runs its own through CheckRole,
// and yarctl config check runs them all without starting anything.
var roleChecks = map[string]func(c *Config) error{
	"yarilo-auth": func(c *Config) error { return c.Auth.Token.check() },
	"yarilo-backend-api": func(c *Config) error {
		if _, _, err := c.BackendAPI.Gate(); err != nil {
			return err
		}
		if err := c.LocksClient.Check(false); err != nil {
			return err
		}
		return c.Storage.checkCryptKeys()
	},
	"yarilo-backend": func(c *Config) error {
		if err := c.LocksClient.Check(false); err != nil {
			return err
		}
		if err := c.checkSieveSendRate(); err != nil {
			return err
		}
		return c.Storage.checkCryptKeys()
	},
	"yarilo-director": func(c *Config) error {
		if raw := strings.TrimSpace(c.DirectorService.UsernameHash); raw != "" {
			if _, err := ring.ParseHashFormat(raw); err != nil {
				return fmt.Errorf("director_service.username_hash %q: %w", raw, err)
			}
		}
		_, _, err := c.DirectorService.API.Gate()
		return err
	},
	"yarilo-fts": func(c *Config) error {
		f := c.FTS
		if err := language.ValidateTokenizerConfig(f.LanguageTokenizerAlgorithm, f.LanguageTokenizerWB5A, f.LanguageTokenizerExplicitPrefix); err != nil {
			return err
		}
		return c.Storage.checkCryptKeys()
	},
	"yarilo-imap-login": func(c *Config) error {
		return ValidateBackendOrDirector("imap_login_service", c.IMAPLoginService.BackendAddr, c.IMAPLoginService.DirectorAddr)
	},
	"yarilo-pop3-login": func(c *Config) error {
		return ValidateBackendOrDirector("pop3_login_service", c.POP3LoginService.BackendAddr, c.POP3LoginService.DirectorAddr)
	},
	"yarilo-submission-login": func(c *Config) error {
		return ValidateBackendOrDirector("submission_login_service", c.SubmissionLoginSvc.BackendAddr, c.SubmissionLoginSvc.DirectorAddr)
	},
	"yarilo-managesieve-login": func(c *Config) error {
		return ValidateBackendOrDirector("managesieve_login_service", c.ManageSieveLoginService.BackendAddr, c.ManageSieveLoginService.DirectorAddr)
	},
	"yarilo-jmap-login": func(c *Config) error {
		if err := ValidateBackendOrDirector("jmap_login_service", c.JMAPLoginService.BackendAddr, c.JMAPLoginService.DirectorAddr); err != nil {
			return err
		}
		_, err := CheckTicketKey(c.JMAPLoginService.WebSocketTicketKey, c.Protocol.JMAP.WebSocketTicketTTL)
		return err
	},
	"yarilo-jmap": func(c *Config) error {
		if err := c.LocksClient.Check(true); err != nil {
			return err
		}
		return c.Storage.checkCryptKeys()
	},
	"yarilo-locks": func(c *Config) error { return c.LocksService.check() },
}

// roleSections names the keys of the file whose presence says the role is
// deployed: the chart renders a login service's section only when it is on,
// managesieve's address keys only (its section carries haproxy_timeout always).
var roleSections = map[string][]string{
	"yarilo-auth":              {"auth"},
	"yarilo-backend-api":       {"backend_api"},
	"yarilo-backend":           {"locks_client"},
	"yarilo-director":          {"director_service"},
	"yarilo-fts":               {"fts"},
	"yarilo-imap-login":        {"imap_login_service"},
	"yarilo-pop3-login":        {"pop3_login_service"},
	"yarilo-submission-login":  {"submission_login_service"},
	"yarilo-managesieve-login": {"managesieve_login_service.backend_addr", "managesieve_login_service.director_addr"},
	"yarilo-jmap-login":        {"jmap_login_service"},
	"yarilo-jmap":              {"jmap_service"},
	"yarilo-locks":             {"locks_service"},
}

// componentRoles maps the YARILO_COMPONENT a chart container carries to the
// role checks its binary runs at start; a component with none only loads.
// The session binaries and the migration build the store as the backend role
// does; submission relays and builds none.
var componentRoles = map[string][]string{
	"yarilo-auth":              {"yarilo-auth"},
	"yarilo-backend-api":       {"yarilo-backend-api"},
	"yarilo-backend-reg":       nil,
	"yarilo-dict":              nil,
	"yarilo-director":          {"yarilo-director"},
	"yarilo-fts":               {"yarilo-fts"},
	"yarilo-imap":              {"yarilo-backend"},
	"yarilo-imap-login":        {"yarilo-imap-login"},
	"yarilo-jmap":              {"yarilo-jmap"},
	"yarilo-jmap-login":        {"yarilo-jmap-login"},
	"yarilo-lmtp":              {"yarilo-backend"},
	"yarilo-lmtp-login":        nil,
	"yarilo-locks":             {"yarilo-locks"},
	"yarilo-managesieve":       {"yarilo-backend"},
	"yarilo-managesieve-login": {"yarilo-managesieve-login"},
	"yarilo-migrate":           {"yarilo-backend"},
	"yarilo-pop3":              {"yarilo-backend"},
	"yarilo-pop3-login":        {"yarilo-pop3-login"},
	"yarilo-quota-status":      nil,
	"yarilo-sasl-login":        nil,
	"yarilo-submission":        nil,
	"yarilo-submission-login":  {"yarilo-submission-login"},
	"yarilo-warden":            nil,
}

// ComponentRoles are the role checks of a chart component; an unknown one is
// an error naming the known components.
func ComponentRoles(component string) ([]string, error) {
	roles, ok := componentRoles[component]
	if !ok {
		known := make([]string, 0, len(componentRoles))
		for c := range componentRoles {
			known = append(known, c)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("config: no component %q (known: %s)", component, strings.Join(known, ", "))
	}
	return roles, nil
}

// PresentRoles are the roles whose section the file at path carries.
func PresentRoles(path string) ([]string, error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range Roles() {
		for _, key := range roleSections[r] {
			if k.Exists(key) {
				out = append(out, r)
				break
			}
		}
	}
	return out, nil
}

// Roles names the roles CheckRoles knows, sorted.
func Roles() []string {
	out := make([]string, 0, len(roleChecks))
	for r := range roleChecks {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// CheckRole runs one role's start checks; an unknown role is an error.
func (c *Config) CheckRole(role string) error {
	check, ok := roleChecks[role]
	if !ok {
		return fmt.Errorf("config: no role %q (known: %s)", role, strings.Join(Roles(), ", "))
	}
	if err := check(c); err != nil {
		return fmt.Errorf("config: %s: %w", role, err)
	}
	return nil
}

// CheckRoles runs the named roles' checks, every role when none is named,
// and reports every refusal, not the first.
func (c *Config) CheckRoles(roles ...string) error {
	if len(roles) == 0 {
		roles = Roles()
	}
	var errs []error
	for _, r := range roles {
		if err := c.CheckRole(r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// LoadChecked is Load plus the start checks of the named roles, or of every
// role whose section the file carries: what a start would refuse, without
// starting.
func LoadChecked(path string, roles ...string) (*Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if len(roles) == 0 {
		if roles, err = PresentRoles(path); err != nil {
			return nil, err
		}
		if len(roles) == 0 {
			return cfg, nil
		}
	}
	if err := cfg.CheckRoles(roles...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Check is what a client of yarilo-locks refuses at start; required says the
// role cannot run without one.
func (lc LocksClientConfig) Check(required bool) error {
	switch lc.Mode {
	case "":
		if required {
			return errors.New("locks_client is not configured; set mode remote (or embedded for a single-node dev run)")
		}
	case "embedded":
		if lc.Socket == "" {
			return errors.New("locks_client.socket is required for embedded mode")
		}
	case "remote":
		if len(lc.Endpoints) == 0 {
			return errors.New("locks_client.endpoints must list at least one host:port for remote mode")
		}
	default:
		return fmt.Errorf("locks_client: unknown mode %q (want remote | embedded | \"\")", lc.Mode)
	}
	return nil
}

func (l LocksServiceConfig) check() error {
	switch l.Mode {
	case "embedded":
		if l.Socket == "" {
			return errors.New("locks_service.socket is required for embedded mode")
		}
	case "remote":
		if l.Listen == "" {
			return errors.New("locks_service.listen is required for remote mode")
		}
		if l.Redis == "" {
			return errors.New("locks_service.redis is required for remote mode")
		}
	default:
		return fmt.Errorf("unknown locks_service.mode %q (want embedded | remote)", l.Mode)
	}
	return nil
}

func (t AuthTokenConfig) check() error {
	if t.Backend != "redis" {
		return nil
	}
	if t.RedisAddr == "" {
		return errors.New("auth.token.backend=redis requires auth.token.redis_addr")
	}
	if _, err := redisopt.Parse(t.RedisAddr, t.RedisPassword); err != nil {
		return fmt.Errorf("auth.token.redis_addr invalid: %w", err)
	}
	return nil
}

// CheckTicketKey refuses an unexpanded WebSocket ticket key and warns when
// tickets are on with no key.
func CheckTicketKey(key string, ttl int) (string, error) {
	if strings.Contains(key, "${") {
		return "", errors.New("jmap_login_service.websocket_ticket_key is an unexpanded placeholder")
	}
	if ttl > 0 && key == "" {
		return "jmap-login: jmap_login_service.websocket_ticket_key is empty, no WebSocket tickets are issued", nil
	}
	return "", nil
}

// checkCryptKeys reads the storage.mail_crypt key files with the function a
// store-building start uses (mailboxbuild.CryptLayer), so check refuses what
// that start would.
func (s StorageConfig) checkCryptKeys() error {
	if _, err := mailcrypt.LoadKeys(s.MailCryptKeys()); err != nil {
		return fmt.Errorf("storage.mail_crypt.%w", err)
	}
	return nil
}

// checkSieveSendRate refuses a send limit its counter store cannot keep: the
// count lives where the duplicate test keeps its state, a home-dir file or the
// sieve_duplicate dict, and only the redis driver counts in one step there.
// The memory driver counts per process, so each pod would allow the limit.
func (c *Config) checkSieveSendRate() error {
	n := c.Sieve.SendRateLimit
	if n < 0 {
		return fmt.Errorf("sieve.sieve_send_rate_limit %d: want 0 (off) or more", n)
	}
	if n == 0 {
		return nil
	}
	switch c.Sieve.DuplicateDriver {
	case "", "file":
		return nil
	case "redis":
		dc, ok := c.Dicts["sieve_duplicate"]
		if !ok {
			return errors.New("sieve.sieve_send_rate_limit with sieve_duplicate_driver redis needs dicts.sieve_duplicate")
		}
		if dc.Driver != "redis" {
			return fmt.Errorf("sieve.sieve_send_rate_limit needs dicts.sieve_duplicate on the redis driver, which counts in one step; it is on %q", dc.Driver)
		}
		return nil
	}
	return fmt.Errorf("sieve.sieve_send_rate_limit with sieve_duplicate_driver %q would count per process; use file or redis", c.Sieve.DuplicateDriver)
}
