package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
)

// MailCryptKeys is what storage.mail_crypt names, for mailcrypt.LoadKeys.
func (s StorageConfig) MailCryptKeys() mailcrypt.KeyFiles {
	return mailcrypt.KeyFiles{
		Write:         s.MailCrypt.Write,
		PublicKey:     s.MailCrypt.PublicKey,
		PrivateKeys:   s.MailCrypt.PrivateKeys,
		ReferenceKeys: s.MailCrypt.ReferenceKeys,
	}
}

// checkMailCrypt refuses at load what would fail a read or a write later:
// unreadable keys, a cache size that does not parse, and a temp directory
// on the mail volume, where decrypted bodies would sit beside the ciphertext.
func checkMailCrypt(s StorageConfig) error {
	if _, err := mailcrypt.LoadKeys(s.MailCryptKeys()); err != nil {
		return fmt.Errorf("config: storage.mail_crypt.%w", err)
	}
	if _, err := resolveSize("storage.mail_crypt.reference_cache_size", s.MailCrypt.ReferenceCacheSize); err != nil {
		return err
	}
	dir := s.MailTempDir
	if dir == "" || !filepath.IsAbs(dir) {
		return fmt.Errorf("config: storage.mail_temp_dir: %q is not an absolute path", dir)
	}
	dir = filepath.Clean(dir)
	for name, loc := range map[string]string{"maildir_root": s.MaildirRoot, "mail_home": s.MailHome} {
		if root := staticPrefix(loc); root != "" && mailcrypt.Under(dir, root) {
			return fmt.Errorf("config: storage.mail_temp_dir: %s is under storage.%s (%s): decrypted mail would land on the mail volume", dir, name, loc)
		}
	}
	return nil
}

// staticPrefix is the directory part of a location template before its
// first variable: "/var/mail/%d/%n" is "/var/mail"; a relative one has none.
func staticPrefix(loc string) string {
	if i := strings.IndexByte(loc, '%'); i >= 0 {
		loc = loc[:i]
	}
	if !filepath.IsAbs(loc) {
		return ""
	}
	return filepath.Clean(loc)
}
