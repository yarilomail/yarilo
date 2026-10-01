package main

import (
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/internal/backend"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/config"
)

// The admin API's index takes the storage settings from the config, purge share
// and lock method included, not the index's defaults.
func TestTheAdminAPIIndexTakesTheConfiguredOptions(t *testing.T) {
	sc := config.StorageConfig{
		MailCachePurgeDeletePercentage: 33,
		LockMethod:                     "dotlock",
		MailFsync:                      "always",
	}
	got := buildIndex(sc, nil)
	if reflect.DeepEqual(got, file.New()) {
		t.Fatal("the configured index equals the default one, so this row proves nothing")
	}
	if want := file.New(backend.IndexOptions(sc, nil)...); !reflect.DeepEqual(got, want) {
		t.Errorf("the admin API's index is not built from the storage settings:\n got %+v\nwant %+v", got, want)
	}
}
