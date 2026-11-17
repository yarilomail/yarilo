package mailboxbuild

import (
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/quota"
)

var (
	cryptMu     sync.Mutex
	cryptLayers = map[string]*mailcrypt.Layer{}
	// exitProcess stops a process whose keys cannot be read; tests replace it.
	exitProcess = func(err error) {
		slog.Error("storage.mail_crypt: keys unreadable, refusing to start", "err", err)
		os.Exit(1)
	}
)

// CryptLayer is the process's layer for sc, built once, where the store is
// built: the reference cache sweeps its directory when made, so a second one
// would delete the first one's files. Keys that cannot be read stop the
// process at this first call, at start, rather than write plaintext.
func CryptLayer(sc config.StorageConfig) *mailcrypt.Layer {
	key := fmt.Sprintf("%#v|%s", sc.MailCrypt, sc.MailTempDir)
	cryptMu.Lock()
	defer cryptMu.Unlock()
	if l, ok := cryptLayers[key]; ok {
		return l
	}
	l, err := buildCryptLayer(sc)
	if err != nil {
		exitProcess(err)
		return nil
	}
	cryptLayers[key] = l
	return l
}
func buildCryptLayer(sc config.StorageConfig) (*mailcrypt.Layer, error) {
	o, err := mailcrypt.LoadKeys(sc.MailCryptKeys())
	if err != nil {
		return nil, fmt.Errorf("storage.mail_crypt.%w", err)
	}
	if size := quota.ParseSize(sc.MailCrypt.ReferenceCacheSize); size > 0 && len(o.ReferenceKeys) > 0 {
		c, err := mailcrypt.NewCache(sc.MailTempDir, size)
		if err != nil {
			return nil, err
		}
		o.Cache = c
	}
	return mailcrypt.New(o), nil
}
