package guard_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

// renderWith renders the chart defaults plus one values text. Internal TLS off:
// minting a CA and a certificate per role is most of a render's cost.
func renderWith(t *testing.T, extra string) ([]byte, error) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "extra.yaml")
	if err := os.WriteFile(f, []byte(extra), 0o600); err != nil {
		t.Fatal(err)
	}
	return exec.Command("helm", "template", "yarilo", "../../helm",
		"--set", "internalTLS.enabled=false", "-f", f).CombinedOutput()
}

// loadRendered loads the rendered ConfigMap the way a pod does.
func loadRendered(t *testing.T, out []byte) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(path, []byte(renderedConfig(t, string(out))), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("a pod given this configuration refuses to start: %v", err)
	}
	return cfg
}

// An explicit 0, false or "" reaches the config instead of the chart's default,
// and a namespace without list, or with an unquoted yes, still loads (#2162, #2164).
func TestZeroFalseAndUnsetReachTheConfig(t *testing.T) {
	out, err := renderWith(t, `
acl:
  enabled: true
components:
  auth:
    failure_delay: 0
    internal_failure_delay_ms: 0
    policy:
      url: https://policy.example
      hash_nonce: n
      hash_truncate_bits: 0
      check_before: false
      check_after: false
      report_after: false
    master_users:
      separator: ""
namespaces:
  - type: personal
    prefix: ""
    separator: "/"
    inbox: true
  - type: shared
    prefix: "Public/"
    separator: "/"
    list: yes
    location: "maildir:/var/mail/vhosts/public"
`)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	cfg := loadRendered(t, out)
	a := cfg.Auth
	checks := []struct {
		name      string
		got, want any
	}{
		{"auth_failure_delay", a.FailureDelaySeconds, 0},
		{"internal_failure_delay_ms", a.InternalFailureDelayMs, 0},
		{"auth_policy_hash_truncate", a.Policy.HashTruncateBits, uint(0)},
		{"auth_policy_check_before_auth", a.Policy.CheckBefore, false},
		{"auth_policy_check_after_auth", a.Policy.CheckAfter, false},
		{"auth_policy_report_after_auth", a.Policy.ReportAfter, false},
		{"auth_master_user_separator", a.MasterUsers.Separator, ""},
		{"namespaces[1].list", cfg.Namespaces[1].List, "yes"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if strings.Contains(renderedConfig(t, string(out)), "list: false") {
		t.Error("a namespace without list renders list: false")
	}
}

// The SQL pool settings of a passdb entry reach the config.
func TestPassdbPoolSettingsReachTheConfig(t *testing.T) {
	out, err := renderWith(t, `
components:
  auth:
    passdb:
      - driver: static
        static_password: "x"
        max_open_conns: 7
        max_idle_conns: 3
        conn_max_lifetime: 120
        conn_max_idle_time: 30
`)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	p := loadRendered(t, out).Auth.Passdb[0]
	got := []*int{p.MaxOpenConns, p.MaxIdleConns, p.ConnMaxLifetime, p.ConnMaxIdleTime}
	for i, want := range []int{7, 3, 120, 30} {
		if got[i] == nil || *got[i] != want {
			t.Errorf("pool setting %d = %v, want %d", i, got[i], want)
		}
	}
}

// With quota-status on and no address of its own, it looks recipients up on the
// release's auth service rather than accepting every one (#2160).
func TestQuotaStatusLooksUpOnTheReleaseAuth(t *testing.T) {
	out, err := renderWith(t, "components:\n  quotaStatus:\n    enabled: true\n    auth_master_addr: \"\"\n")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if got := loadRendered(t, out).QuotaStatus.AuthMasterAddr; !strings.HasPrefix(got, "yarilo-auth.") || !strings.HasSuffix(got, ":9102") {
		t.Errorf("quota_status.auth_master_addr = %q, want the release's auth master service", got)
	}
}

// canonicalNames are the yarilo key names the chart refuses where it reads its
// own name for the same setting, with that name and where the entry sits.
var canonicalNames = []struct{ at, yarilo, chart string }{
	{"components.auth", "auth_failure_delay", "failure_delay"},
	{"components.auth.master_users", "auth_master_user_separator", "separator"},
	{"quota", "quota_storage_grace", "quota_grace"},
	{"components.auth.policy", "auth_policy_server_url", "url"},
	{"components.auth.policy", "auth_policy_server_api_header", "api_header"},
	{"components.auth.policy", "auth_policy_hash_mech", "hash_mech"},
	{"components.auth.policy", "auth_policy_hash_nonce", "hash_nonce"},
	{"components.auth.policy", "auth_policy_hash_truncate", "hash_truncate_bits"},
	{"components.auth.policy", "auth_policy_reject_on_fail", "reject_on_fail"},
	{"components.auth.policy", "auth_policy_log_only", "log_only"},
	{"components.auth.policy", "auth_policy_check_before_auth", "check_before"},
	{"components.auth.policy", "auth_policy_check_after_auth", "check_after"},
	{"components.auth.policy", "auth_policy_report_after_auth", "report_after"},
	{"components.auth.passdb[0]", "passwd_file_path", "passwd_file"},
	{"components.auth.passdb[0]", "passdb_sql_query", "password_query"},
	{"components.auth.passdb[0]", "userdb_sql_query", "user_query"},
	{"components.auth.passdb[0]", "userdb_sql_iterate_query", "iterate_query"},
	{"components.auth.passdb[0]", "passdb_default_password_scheme", "default_pass_scheme"},
	{"components.auth.master_users.masterdb[0]", "passwd_file_path", "passwd_file"},
	{"components.auth.master_users.masterdb[0]", "passdb_sql_query", "password_query"},
	{"components.auth.master_users.masterdb[0]", "userdb_sql_query", "user_query"},
	{"components.auth.master_users.masterdb[0]", "userdb_sql_iterate_query", "iterate_query"},
	{"components.auth.master_users.masterdb[0]", "passdb_default_password_scheme", "default_pass_scheme"},
}

func init() {
	for _, n := range []string{"mode", "jwks_url", "introspection_url", "tokeninfo_url", "issuer_url", "introspection_mode",
		"prefer_introspection", "client_id", "client_secret", "issuers", "audience", "username_attribute",
		"username_validation_format", "active_attribute", "active_value", "token_expire_grace_seconds", "http_timeout_ms"} {
		canonicalNames = append(canonicalNames, struct{ at, yarilo, chart string }{"components.auth.oauth2[0]", "oauth2_" + n, n})
	}
	canonicalNames = append(canonicalNames,
		struct{ at, yarilo, chart string }{"components.auth.oauth2[0]", "oauth2_scope", "scopes"},
		struct{ at, yarilo, chart string }{"components.auth.oauth2[0]", "oauth2_fields", "extra_fields"})
}

// valuesAt writes key: value at a dotted path, an [0] step being a one-entry list.
func valuesAt(at, key, value string) string {
	var b strings.Builder
	indent := ""
	for _, step := range strings.Split(at, ".") {
		list := strings.HasSuffix(step, "[0]")
		step = strings.TrimSuffix(step, "[0]")
		fmt.Fprintf(&b, "%s%s:\n", indent, step)
		indent += "  "
		if list {
			fmt.Fprintf(&b, "%s- driver: static\n", indent)
			indent += "  "
		}
	}
	fmt.Fprintf(&b, "%s%s: %s\n", indent, key, value)
	return b.String()
}

// Each yarilo name the chart would drop fails the render, naming both spellings
// and where it sits; the chart's name carries the value instead (#2161, #2162).
func TestCanonicalNamesAreRefused(t *testing.T) {
	if len(canonicalNames) < 40 {
		t.Fatalf("only %d names: the table lost its oauth2 rows", len(canonicalNames))
	}
	for _, c := range canonicalNames {
		t.Run(c.at+"."+c.yarilo, func(t *testing.T) {
			t.Parallel()
			out, err := renderWith(t, valuesAt(c.at, c.yarilo, `"v"`))
			if err == nil {
				t.Fatalf("%s.%s rendered; the chart drops it without a word", c.at, c.yarilo)
			}
			want := fmt.Sprintf("%s.%s is not read; the chart expects %s", c.at, c.yarilo, c.chart)
			if !strings.Contains(string(out), want) {
				t.Errorf("the failure does not say %q:\n%s", want, out)
			}
			// The chart's own name carries the same value into the config.
			out, err = renderWith(t, valuesAt(c.at, c.chart, `"v-`+c.chart+`"`))
			if err != nil {
				t.Fatalf("helm template with %s: %v\n%s", c.chart, err, out)
			}
			wantLine := "v-" + c.chart // a block scalar puts it on the next line
			if c.chart == "prefer_introspection" {
				wantLine = "true"
			}
			found := false
			lines := strings.Split(renderedConfig(t, string(out)), "\n")
			for i, line := range lines {
				if strings.Contains(line, c.yarilo+":") &&
					(strings.Contains(line, wantLine) || i+1 < len(lines) && strings.Contains(lines[i+1], wantLine)) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s.%s did not reach the config as %s: %s", c.at, c.chart, c.yarilo, wantLine)
			}
		})
	}
}

// The chart renders each zero-is-off key with its real default, so a release
// that sets nothing keeps the defaults, and a 0 in the values turns it off.
func TestChartZeroIsOff(t *testing.T) {
	out, err := renderWith(t, "")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	def := loadRendered(t, out)
	out, err = renderWith(t, `
login: {transientRetries: 0, transientReloginCap: 0}
internalTLS: {sessionCacheSize: 0}
components:
  auth: {client: {pool_size: 0, pool_idle_timeout: 0}, startup_wait: 0}
  backend: {threading: {cache_idle: 0}}
  locks: {client: {startup_wait: 0}}
  director: {write_timeout: 0, user_kick_delay: 0, max_parallel_kicks: 0, anti_entropy_interval: 0, tombstone_ttl: 0}
storage: {storage_lock_stale_timeout: 0, mail_cache_purge_delete_percentage: 0, mail_cache_purge_continued_percentage: 0}
`)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	off := loadRendered(t, out)
	rows := []struct {
		name     string
		def, off int
		wantDef  int
	}{
		{"threading_cache_idle", def.Threading.ThreadingCacheIdle, off.Threading.ThreadingCacheIdle, 300},
		{"auth_client_pool_size", def.AuthClient.PoolSize, off.AuthClient.PoolSize, 4},
		{"auth_client_pool_idle_timeout", def.AuthClient.PoolIdleTimeoutSecs, off.AuthClient.PoolIdleTimeoutSecs, 300},
		{"transient_retries", def.Login.TransientRetries, off.Login.TransientRetries, 3},
		{"transient_relogin_cap", def.Login.TransientReloginCap, off.Login.TransientReloginCap, 3},
		{"session_cache_size", def.InternalTLS.SessionCacheSize, off.InternalTLS.SessionCacheSize, 64},
		{"auth_startup_wait", def.AuthService.StartupWaitSeconds, off.AuthService.StartupWaitSeconds, 30},
		{"locks_client_startup_wait", def.LocksClient.StartupWaitSeconds, off.LocksClient.StartupWaitSeconds, 30},
		{"storage_lock_stale_timeout", def.Storage.LockStaleTimeout, off.Storage.LockStaleTimeout, 180},
		{"mail_cache_purge_delete_percentage", def.Storage.MailCachePurgeDeletePercentage, off.Storage.MailCachePurgeDeletePercentage, 20},
		{"mail_cache_purge_continued_percentage", def.Storage.MailCachePurgeContinuedPercentage, off.Storage.MailCachePurgeContinuedPercentage, 200},
		{"write_timeout", def.DirectorService.WriteTimeout, off.DirectorService.WriteTimeout, 10},
		{"user_kick_delay", def.DirectorService.UserKickDelay, off.DirectorService.UserKickDelay, 2},
		{"max_parallel_kicks", def.DirectorService.MaxParallelKicks, off.DirectorService.MaxParallelKicks, 100},
		{"anti_entropy_interval", def.DirectorService.AntiEntropyInterval, off.DirectorService.AntiEntropyInterval, 3},
		{"tombstone_ttl", def.DirectorService.TombstoneTTL, off.DirectorService.TombstoneTTL, 600},
	}
	for _, r := range rows {
		if r.def != r.wantDef {
			t.Errorf("%s with chart defaults = %d, want %d", r.name, r.def, r.wantDef)
		}
		if r.off != 0 {
			t.Errorf("%s with 0 in the values = %d, want 0", r.name, r.off)
		}
	}
}
