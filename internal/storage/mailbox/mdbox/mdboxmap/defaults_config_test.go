package mdboxmap

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// Open takes an empty format as FormatV2.
func TestDefaultsAreTheEmptyFormat(t *testing.T) {
	if got := config.Defaults().Storage.MdboxMapFormat; got != string(FormatV2) {
		t.Fatalf("defaults %q, empty gives %q", got, FormatV2)
	}
}
