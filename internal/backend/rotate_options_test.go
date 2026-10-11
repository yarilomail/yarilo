package backend

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

// The rotation triple reaches the index as given, zeros included: a 0 is the
// operator turning an arm off, not a request for a built-in value.
func TestIndexOptionsAlwaysForwardTheRotationTriple(t *testing.T) {
	zero := len(IndexOptions(config.StorageConfig{}, nil))
	set := len(IndexOptions(config.StorageConfig{MailIndexLogRotateMinSize: 8 << 10,
		MailIndexLogRotateMaxSize: 64 << 10, MailIndexLogRotateMinAge: 60}, nil))
	if zero != set {
		t.Errorf("options: %d with the triple at 0, %d with it set -- a 0 is being dropped before the index", zero, set)
	}
}
