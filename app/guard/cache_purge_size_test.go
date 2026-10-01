package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

// A zero minimum written without quotes renders as "0" and loads as zero: a
// default filter would read it as unset and render the 32 KiB it disables.
func TestACacheMinimumOfZeroReachesTheConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values string
		want   int64
	}{
		{"zero without quotes", "storage:\n  mail_cache_purge_min_size: 0\n", 0},
		{"the chart default", "", 32 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"template", "../../helm"}
			if tc.values != "" {
				vf := filepath.Join(t.TempDir(), "values.yaml")
				if err := os.WriteFile(vf, []byte(tc.values), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "-f", vf)
			}
			out, err := exec.Command("helm", args...).Output()
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			path := filepath.Join(t.TempDir(), "yarilo.yaml")
			if err := os.WriteFile(path, []byte(renderedConfig(t, string(out))), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Storage.MailCachePurgeMinSize; got != tc.want || cfg.Storage.MailCachePurgeMinSizeRaw == "" {
				t.Errorf("the pod loads a minimum of %d (raw %q), want %d", got, cfg.Storage.MailCachePurgeMinSizeRaw, tc.want)
			}
		})
	}
}
