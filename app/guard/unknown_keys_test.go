package guard_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// The loader refuses a key nothing reads (#2567). The chart's renders are
// loaded by TestEveryKeptValuesFileLoads; these are the yarilo.yaml files that
// ship as they are, which nothing else loads.
func TestNoShippedConfigCarriesAnUnknownKey(t *testing.T) {
	for _, f := range []string{"config/yarilo.yaml", "deploy/compose/config/yarilo.yaml", "deploy/systemd/yarilo.yaml"} {
		unknown, err := config.UnknownKeys(filepath.Join("../..", f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if len(unknown) > 0 {
			t.Errorf("%s carries keys nothing reads: %s", f, strings.Join(unknown, ", "))
		}
	}
}
