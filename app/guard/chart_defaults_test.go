package guard_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

// chartDiffers are the keys whose chart value is not the binary's on purpose.
var chartDiffers = map[string]string{
	"director_service.director_domain_rebalance_percent": "documented as 0 in the binary, 20 in the chart",
	"config_schema_version":                              "stamped by the chart",
}

// numbersByPath flattens every numeric setting to its config path.
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
	}
}

// values.yaml carries the binary's defaults: a release that sets nothing loads
// what config.Defaults() holds, with every optional component off and on.
func TestTheChartCarriesTheBinaryDefaults(t *testing.T) {
	want := map[string]float64{}
	numbersByPath(reflect.ValueOf(config.Defaults()).Elem(), "", want)
	allOn := "fts:\n  enabled: true\ncomponents:\n  saslLogin:\n    enabled: true\n  manageSieveLogin:\n    enabled: true\n  quotaStatus:\n    enabled: true\n  dict:\n    enabled: true\n"
	for _, extra := range []string{"", allOn} {
		got := map[string]float64{}
		numbersByPath(reflect.ValueOf(loadRendered(t, mustRender(t, extra))).Elem(), "", got)
		for path, w := range want {
			if _, ok := chartDiffers[path]; ok {
				continue
			}
			if got[path] != w {
				t.Errorf("%q: %s = %g in the chart, %g in config.Defaults()", extra, path, got[path], w)
			}
		}
	}
	cfg, d := loadRendered(t, mustRender(t, "")), config.Defaults()
	for _, row := range []struct{ path, got, want string }{
		{"storage.mdbox_rotate_size", cfg.Storage.MdboxRotateSize, d.Storage.MdboxRotateSize},
		{"storage.mail_index_log_rotate_min_size", cfg.Storage.MailIndexLogRotateMinSizeRaw, d.Storage.MailIndexLogRotateMinSizeRaw},
		{"storage.mail_index_log_rotate_max_size", cfg.Storage.MailIndexLogRotateMaxSizeRaw, d.Storage.MailIndexLogRotateMaxSizeRaw},
		{"fts.fts_detection_sample_bytes", cfg.FTS.DetectionSampleBytesRaw, d.FTS.DetectionSampleBytesRaw},
	} {
		if row.got != row.want {
			t.Errorf("%s = %q in the chart, %q in config.Defaults()", row.path, row.got, row.want)
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
