package guard_test

import (
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// chartDiffers are the keys whose chart value is not the binary's on purpose.
var chartDiffers = map[string]string{
	"director_service.director_domain_rebalance_percent": "documented as 0 in the binary, 20 in the chart",
	"config_schema_version":                              "stamped by the chart",
	"backend_register.readiness_protocols":               "the protocol components the release enables in the pod",
	"locks_client.endpoints":                             "the release's own yarilo-locks Service",
}

// setByTheRender are values this test's own renders choose, keyed by the extra values text.
var setByTheRender = map[string]map[string]string{
	"":    {"internal_tls.enabled": "renderWith turns internal TLS off"},
	allOn: {"internal_tls.enabled": "renderWith turns internal TLS off", "fts.enabled": "allOn turns FTS on"},
}

const allOn = "fts:\n  enabled: true\ncomponents:\n  saslLogin:\n    enabled: true\n  manageSieveLogin:\n    enabled: true\n  quotaStatus:\n    enabled: true\n  dict:\n    enabled: true\n"

// numbersByPath flattens every numeric and boolean setting to its config path; a bool is 0 or 1.
func numbersByPath(v reflect.Value, path string, out map[string]float64) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			numbersByPath(v.Elem(), path, out)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			tag := strings.Split(f.Tag.Get("koanf"), ",")[0]
			if tag == "" || tag == "-" || !f.IsExported() {
				continue
			}
			numbersByPath(v.Field(i), strings.TrimPrefix(path+"."+tag, "."), out)
		}
	case reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint32, reflect.Uint64, reflect.Float64:
		var n float64
		fmt.Sscan(fmt.Sprint(v.Interface()), &n)
		out[path] = n
	case reflect.Bool:
		out[path] = 0
		if v.Bool() {
			out[path] = 1
		}
	}
}

// listsByPath flattens every list of strings to its config path; nil and empty are one value.
func listsByPath(v reflect.Value, path string, out map[string][]string) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			listsByPath(v.Elem(), path, out)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			tag := strings.Split(f.Tag.Get("koanf"), ",")[0]
			if tag == "" || tag == "-" || !f.IsExported() {
				continue
			}
			listsByPath(v.Field(i), strings.TrimPrefix(path+"."+tag, "."), out)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.String {
			l := make([]string, v.Len())
			for i := range l {
				l[i] = v.Index(i).String()
			}
			out[path] = l
		}
	}
}

// values.yaml carries the binary's defaults: a release that sets nothing loads
// what config.Defaults() holds, with every optional component off and on.
func TestTheChartCarriesTheBinaryDefaults(t *testing.T) {
	want := map[string]float64{}
	numbersByPath(reflect.ValueOf(config.Defaults()).Elem(), "", want)
	wantLists := map[string][]string{}
	listsByPath(reflect.ValueOf(config.Defaults()).Elem(), "", wantLists)
	for _, extra := range []string{"", allOn} {
		rendered := loadRendered(t, mustRender(t, extra))
		got := map[string]float64{}
		numbersByPath(reflect.ValueOf(rendered).Elem(), "", got)
		gotLists := map[string][]string{}
		listsByPath(reflect.ValueOf(rendered).Elem(), "", gotLists)
		for path, w := range wantLists {
			if _, ok := chartDiffers[path]; ok {
				continue
			}
			if g := gotLists[path]; strings.Join(g, "\x00") != strings.Join(w, "\x00") {
				t.Errorf("%q: %s = %q in the chart, %q in config.Defaults()", extra, path, g, w)
			}
		}
		for path, w := range want {
			if _, ok := chartDiffers[path]; ok {
				continue
			}
			if _, ok := setByTheRender[extra][path]; ok {
				continue
			}
			if got[path] != w {
				t.Errorf("%q: %s = %g in the chart, %g in config.Defaults()", extra, path, got[path], w)
			}
		}
	}
	cfg, d := loadRendered(t, mustRender(t, "")), config.Defaults()
	jmap := loadRendered(t, mustRender(t, "components:\n  jmapLogin:\n    enabled: true\n    backend_addr: \"jmap:10443\"\n"))
	for _, row := range []struct{ path, got, want string }{
		{"storage.mdbox_rotate_size", cfg.Storage.MdboxRotateSize, d.Storage.MdboxRotateSize},
		{"storage.mail_index_log_rotate_min_size", cfg.Storage.MailIndexLogRotateMinSizeRaw, d.Storage.MailIndexLogRotateMinSizeRaw},
		{"storage.mail_index_log_rotate_max_size", cfg.Storage.MailIndexLogRotateMaxSizeRaw, d.Storage.MailIndexLogRotateMaxSizeRaw},
		{"fts.fts_detection_sample_bytes", cfg.FTS.DetectionSampleBytesRaw, d.FTS.DetectionSampleBytesRaw},
		{"auth.auth_username_format", cfg.Auth.UsernameFormat, d.Auth.UsernameFormat},
		{"jmap_login_service.client_ip_header", jmap.JMAPLoginService.ClientIPHeader, d.JMAPLoginService.ClientIPHeader},
	} {
		if row.got != row.want {
			t.Errorf("%s = %q in the chart, %q in config.Defaults()", row.path, row.got, row.want)
		}
	}
}

// listenKeys are the hosts listeners bind. The binary keeps internal services
// to loopback and leaves a services.<name> address empty for the process's
// role; the chart binds every interface, since a pod is reached over its own
// network (#2415, #2416).
var listenKeys = []struct {
	path, binary, chart string
	host                func(*config.Config) string
}{
	{"auth_service.listen", "127.0.0.1", "", func(c *config.Config) string { return listenHost(c.AuthService.Listen) }},
	{"warden_service.listen", "127.0.0.1", "", func(c *config.Config) string { return listenHost(c.WardenService.Listen) }},
	{"director_service.listen", "127.0.0.1", "", func(c *config.Config) string { return listenHost(c.DirectorService.Listen) }},
	{"director_service.api.listen", "127.0.0.1", "", func(c *config.Config) string { return listenHost(c.DirectorService.API.Listen) }},
	{"fts.fts_listen", "127.0.0.1", "", func(c *config.Config) string { return listenHost(c.FTS.Listen) }},
	{"quota_status.listen", "127.0.0.1", "", func(c *config.Config) string { return listenHost(c.QuotaStatus.Listen) }},
	{"sasl_login.listen", "127.0.0.1", "", func(c *config.Config) string { return listenHost(c.SASLLogin.Listen) }},
	{"backend_api.listen", "127.0.0.1", "", func(c *config.Config) string { return listenHost(c.BackendAPI.Listen) }},
	{"services.imaps_login.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.IMAPSLogin) }},
	{"services.imap_login.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.IMAPLogin) }},
	{"services.pop3s_login.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.POP3SLogin) }},
	{"services.pop3_login.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.POP3Login) }},
	{"services.submission_login.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.SubmissionLogin) }},
	{"services.submissions_login.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.SubmissionsLogin) }},
	{"services.lmtp_login.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.LMTPLogin) }},
	{"services.managesieve_login.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.ManageSieveLogin) }},
	{"services.jmap_login.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.JMAPLogin) }},
	{"services.imap.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.IMAP) }},
	{"services.pop3.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.POP3) }},
	{"services.submission.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.Submission) }},
	{"services.lmtp.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.LMTP) }},
	{"services.managesieve.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.ManageSieve) }},
	{"services.jmap.address", "", "0.0.0.0", func(c *config.Config) string { return address(c.Services.JMAP) }},
}

// listenHost is the host of a listen address, or "?" when it has none.
func listenHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "?"
	}
	return host
}

func address(s *config.ServiceConfig) string {
	if s == nil {
		return ""
	}
	return s.Address
}

func TestListenersAreLoopbackInTheBinaryAndOpenInTheChart(t *testing.T) {
	d := config.Defaults()
	cfg := loadRendered(t, mustRender(t, allOn))
	for _, k := range listenKeys {
		if got := k.host(d); got != k.binary {
			t.Errorf("%s: host %q in config.Defaults(), want %q", k.path, got, k.binary)
		}
		if got := k.host(cfg); got != k.chart {
			t.Errorf("%s: host %q in the chart, want %q", k.path, got, k.chart)
		}
	}
}

func mustRender(t *testing.T, extra string) []byte {
	t.Helper()
	out, err := renderWith(t, extra)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return out
}

// The default lookup-hold budget outlasts the director's confirm grace, or a
// concurrent login errors before the kill confirms (#858).
func TestTheHoldBudgetOutlastsTheConfirmGrace(t *testing.T) {
	d := config.Defaults()
	if budgetMs, graceMs := d.Login.LookupHoldMax*d.Login.LookupHoldBackoffMs, d.DirectorService.UserKillConfirmGrace*1000; budgetMs <= graceMs {
		t.Errorf("hold budget %dms does not exceed the %dms confirm grace", budgetMs, graceMs)
	}
}
