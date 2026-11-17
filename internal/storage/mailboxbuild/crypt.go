package mailboxbuild

import (
	"fmt"
	"sync"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/quota"
)

var (
	cryptMu     sync.Mutex
	cryptLayers = map[string]*mailcrypt.Layer{}
)

// CryptLayer is the process's layer for sc, built once: the reference cache
// sweeps its directory when it is made, so a second one would delete the
// first one's files. A key file that stopped parsing after the config load
// gives a layer that refuses encrypted reads and, with write: age, every
// write, rather than writing plaintext.
func CryptLayer(sc config.StorageConfig) *mailcrypt.Layer {
	key := fmt.Sprintf("%#v|%s", sc.MailCrypt, sc.MailTempDir)
	cryptMu.Lock()
	defer cryptMu.Unlock()
	if l, ok := cryptLayers[key]; ok {
		return l
	}
	l, err := buildCryptLayer(sc)
	if err != nil {
		l = mailcrypt.Broken(err, sc.MailCrypt.Write == "age")
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
