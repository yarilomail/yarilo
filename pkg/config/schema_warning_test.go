package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/build"
)

// The schema warning is about one thing, a chart and an image out of step, and
// only chart_version says a chart wrote the file. A hand-written file is told
// nothing; a ConfigMap is told the keys it does not set, never the ones it sets
// to the default (#2453).
func TestTheSchemaWarningNamesOnlyWhatAChartLeftOut(t *testing.T) {
	const chart = "2.4.6-dev.27"
	sample, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", "yarilo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	stale := "chart_version: \"" + chart + "\"\n" +
		"dict_service:\n  socket_mode: \"0660\"\n" +
		"protocol:\n  imap:\n    mail_max_userip_connections: 10\n"
	for _, tc := range []struct {
		name    string
		file    string
		named   []string
		unnamed []string
		silent  bool
	}{
		{name: "the systemd sample, which no chart wrote", file: string(sample), silent: true},
		{name: "a ConfigMap from a chart at schema 21",
			file:    stale + "config_schema_version: 21\n",
			named:   []string{"dict_service.socket_group", "protocol.pop3.mail_max_userip_connections"},
			unnamed: []string{"dict_service.socket_mode", "protocol.imap.mail_max_userip_connections", "hostname"}},
		{name: "a ConfigMap from a chart before the schema",
			file:    stale,
			named:   []string{"hostname", "dict_service.socket_group"},
			unnamed: []string{"chart_version", "dict_service.socket_mode"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prevBuild := build.ChartVersion
			build.ChartVersion = chart
			defer func() { build.ChartVersion = prevBuild }()
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(prev)

			path := filepath.Join(t.TempDir(), "yarilo.yaml")
			if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			skew := strings.Contains(out, "ConfigMap") || strings.Contains(out, "chart_version")
			if tc.silent {
				if skew {
					t.Errorf("a file no chart wrote is told about a chart:\n%s", out)
				}
				return
			}
			m := regexp.MustCompile(`defaulted=(\S+)`).FindStringSubmatch(out)
			if m == nil {
				t.Fatalf("no schema warning:\n%s", out)
			}
			got := map[string]bool{}
			for _, k := range strings.Split(strings.Trim(m[1], `"`), ",") {
				got[k] = true
			}
			for _, k := range tc.named {
				if !got[k] {
					t.Errorf("%s is absent and not named: %s", k, m[1])
				}
			}
			for _, k := range tc.unnamed {
				if got[k] {
					t.Errorf("%s is set and still named", k)
				}
			}
		})
	}
}
