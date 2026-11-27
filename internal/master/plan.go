package master

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// Account is a resolved role user.
type Account struct {
	Name   string
	UID    uint32
	GID    uint32
	Groups []uint32
}

// Component is one process the supervisor runs.
type Component struct {
	Name    string
	Bin     string
	Account Account
	Args    []string // after the binary; the log process's role
	Env     []string // the whole environment it gets
	Ready   []string // absolute socket paths it serves to the ones after it
}

// Plan is a checked configuration: what runs, as whom, with what. Nothing in
// it is read again while the supervisor runs.
type Plan struct {
	Components  []Component
	RunDir      string
	LogPath     string // a directory, or "syslog"
	Facility    string
	StopTimeout time.Duration
	Telemetry   string // the supervisor's own listen address
	ConfigPath  string
	Accounts    map[role]Account
	// GOMAXPROCS every component gets, and whether it came from
	// master_environment or is the default.
	GOMAXPROCS     string
	GOMAXPROCSFrom string
}

// DefaultGOMAXPROCS is what a component gets when master_environment names
// none: on the FreeBSD host 2 was the one value with no stall and the most
// logins, and from 3 up the logins fell to a third and below (#2715,
// reviewer 2026-11-26). master_environment overrides it.
const DefaultGOMAXPROCS = "2"

// Syslog reports whether the components' lines go to syslog.
func (p *Plan) Syslog() bool { return p.LogPath == "syslog" }

// Checker holds what a plan is checked against. TrustedUID is who must own
// the files the supervisor trusts: root, or the test's own uid.
type Checker struct {
	TrustedUID uint32
	Lookup     func(name string) (Account, error)
	BaseEnv    []string // PATH and the like, before the configured variables
	// Self is the supervisor's own binary, which the log process runs.
	Self string
}

// DefaultChecker is the production one.
func DefaultChecker() Checker {
	return Checker{
		TrustedUID: 0,
		Lookup:     lookupAccount,
		BaseEnv:    []string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin"},
		Self:       selfPath(),
	}
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return p
}

func lookupAccount(name string) (Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return Account{}, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return Account{}, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return Account{}, err
	}
	a := Account{Name: name, UID: uint32(uid), GID: uint32(gid)}
	ids, err := u.GroupIds()
	if err != nil {
		return Account{}, err
	}
	for _, id := range ids {
		g, perr := strconv.ParseUint(id, 10, 32)
		if perr != nil {
			return Account{}, perr
		}
		a.Groups = append(a.Groups, uint32(g))
	}
	return a, nil
}

// Build checks cfg and returns the plan, or every reason it cannot start.
// Nothing is started on an error: a typo in the list must not quietly take
// one protocol down.
func (c Checker) Build(cfg *config.Config, configPath string) (*Plan, error) {
	m := cfg.Master
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	p := &Plan{
		RunDir:      m.RunDir,
		LogPath:     m.LogPath,
		Facility:    m.SyslogFacility,
		StopTimeout: time.Duration(m.StopTimeout) * time.Second,
		Telemetry:   fmt.Sprintf("127.0.0.1:%d", m.TelemetryPort),
		ConfigPath:  configPath,
		Accounts:    map[role]Account{},
	}
	if m.StopTimeout <= 0 {
		fail("master_stop_timeout must be above 0")
	}
	if m.TelemetryPort <= 0 || m.TelemetryPort+telemetryOffset[LogComponent] > 65535 {
		fail("master_telemetry_port %d leaves no room for the components' ports", m.TelemetryPort)
	}
	if p.Syslog() {
		if _, ok := facilities[m.SyslogFacility]; !ok {
			fail("master_syslog_facility %q is not a syslog facility", m.SyslogFacility)
		}
	}

	users := map[role]struct{ key, name string }{
		roleInternal: {"master_user", m.User},
		roleAuth:     {"master_auth_user", m.AuthUser},
		roleLogin:    {"master_login_user", m.LoginUser},
		roleDelivery: {"master_delivery_user", m.DeliveryUser},
		roleLog:      {"master_log_user", m.LogUser},
	}
	for r, u := range users {
		a, err := c.Lookup(u.name)
		if err != nil {
			fail("%s %q: %v", u.key, u.name, err)
			continue
		}
		// Only the supervisor runs as root (owner, 2026-11-25).
		if a.UID == 0 || a.GID == 0 {
			fail("%s %q is root; no component runs as root", u.key, u.name)
			continue
		}
		p.Accounts[r] = a
	}

	// The log process opens the files as the log account, so the directory is
	// that account's, not writable by group or others.
	if la, ok := p.Accounts[roleLog]; ok && !p.Syslog() {
		if err := c.ownedBy(m.LogPath, la.UID, true, "master_log_path"); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.trustedFile(c.Self, "the supervisor's own binary, which the log process runs"); err != nil {
		errs = append(errs, err)
	}

	if configPath != "" {
		if err := c.trustedFile(configPath, "the configuration"); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.trustedDir(m.LibexecDir, "master_libexec_dir"); err != nil {
		errs = append(errs, err)
	}
	// The lock, the pid files a new supervisor stops processes by, and the
	// control socket live here. A missing one is made by the supervisor, as
	// root with mode 0755.
	if _, err := os.Lstat(m.RunDir); err == nil {
		if err := c.trustedDir(m.RunDir, "master_run_dir"); err != nil {
			errs = append(errs, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fail("master_run_dir: %v", err)
	}
	if len(m.Components) == 0 {
		fail("master_components is empty")
	}

	extra := make([]string, 0, len(m.Environment)+1)
	p.GOMAXPROCS, p.GOMAXPROCSFrom = DefaultGOMAXPROCS, "default"
	if v, ok := m.Environment["GOMAXPROCS"]; ok {
		p.GOMAXPROCS, p.GOMAXPROCSFrom = v, "master_environment"
	} else {
		extra = append(extra, "GOMAXPROCS="+DefaultGOMAXPROCS)
	}
	for k, v := range m.Environment {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			fail("master_environment has a bad name %q", k)
			continue
		}
		extra = append(extra, k+"="+v)
	}
	sort.Strings(extra)

	// The log process first: it reads every other component's output.
	// No CONFIG: the log account does not read yarilo.yaml and the
	// passwords in it; what the log process needs comes from here.
	logEnv := append([]string{}, c.BaseEnv...)
	logEnv = append(logEnv,
		"LOG_LEVEL="+cfg.Log.Level,
		fmt.Sprintf("TELEMETRY_LISTEN=127.0.0.1:%d", m.TelemetryPort+telemetryOffset[LogComponent]),
		LogPathEnv+"="+m.LogPath,
		LogFacilityEnv+"="+m.SyslogFacility,
		fmt.Sprintf("%s=%t", LogPprofEnv, cfg.Telemetry.PprofEnabled),
		fmt.Sprintf("%s=%d", LogPprofBlockEnv, cfg.Telemetry.PprofBlockProfileRate),
		fmt.Sprintf("%s=%d", LogPprofMutexEnv, cfg.Telemetry.PprofMutexProfileFraction))
	logEnv = append(logEnv, extra...)
	p.Components = append(p.Components, Component{Name: LogComponent, Bin: c.Self, Args: []string{"log"}, Env: logEnv, Account: p.Accounts[roleLog]})

	seen := map[string]bool{}
	for _, name := range m.Components {
		if seen[name] {
			fail("component %q is listed twice in master_components", name)
			continue
		}
		seen[name] = true
		if err := knownComponent(name); err != nil {
			errs = append(errs, err)
			continue
		}
		bin := filepath.Join(m.LibexecDir, "yarilo-"+name)
		if err := c.trustedFile(bin, "component "+name); err != nil {
			errs = append(errs, err)
			continue
		}
		env := append([]string{}, c.BaseEnv...)
		env = append(env,
			"CONFIG="+configPath,
			"LOG_LEVEL="+cfg.Log.Level,
			fmt.Sprintf("TELEMETRY_LISTEN=127.0.0.1:%d", m.TelemetryPort+telemetryOffset[name]))
		env = append(env, extra...)
		if f := envFile(name, m.EnvDir); f != "" {
			vars, err := c.readEnvFile(f)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			env = append(env, vars...)
		}
		comp := Component{Name: name, Bin: bin, Env: env, Account: p.Accounts[roleOf(name)]}
		for _, s := range readySockets(name) {
			comp.Ready = append(comp.Ready, filepath.Join(m.RunDir, s))
		}
		p.Components = append(p.Components, comp)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return p, nil
}

// ownedBy refuses path unless uid owns it and neither group nor others may
// write it; dir says it must be a directory.
func (c Checker) ownedBy(path string, uid uint32, dir bool, what string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uid {
		return fmt.Errorf("%s: %s must be owned by uid %d", what, path, uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s: %s is writable by group or others (%v)", what, path, fi.Mode().Perm())
	}
	if dir && !fi.IsDir() {
		return fmt.Errorf("%s: %s is not a directory", what, path)
	}
	return nil
}

// owned reports whether path is the trusted uid's and not writable by group
// or others. Lstat: a symlink in a trusted place is refused, not followed.
func (c Checker) owned(path string) (os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("%s: no owner to check", path)
	}
	if st.Uid != c.TrustedUID {
		return nil, fmt.Errorf("%s is owned by uid %d, want %d", path, st.Uid, c.TrustedUID)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s is writable by group or others (%v)", path, fi.Mode().Perm())
	}
	return fi, nil
}

func (c Checker) trustedDir(path, what string) error {
	fi, err := c.owned(path)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s: %s is not a directory", what, path)
	}
	return nil
}

func (c Checker) trustedFile(path, what string) error {
	fi, err := c.owned(path)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: %s is not a regular file", what, path)
	}
	return nil
}

// readEnvFile reads NAME=value lines from a password file that must be the
// trusted uid's with mode 0600. A missing file is no error: the component
// then takes its passwords from the configuration.
func (c Checker) readEnvFile(path string) ([]string, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != c.TrustedUID || fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a file owned by uid %d with mode 0600", path, c.TrustedUID)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return nil, fmt.Errorf("%s:%d: not NAME=value", path, n)
		}
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out = append(out, k+"="+v)
	}
	return out, sc.Err()
}
