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

// checkMailCrypt refuses at load what is wrong in the config itself: an
// unknown write mode, a cache size that does not parse, a temp directory on
// the mail volume. Key files are read where a store is built, not here: every
// binary loads this config, and only those that open mail hold the keys.
func checkMailCrypt(s StorageConfig) error {
	switch s.MailCrypt.Write {
	case "off", "age", "":
	default:
		return fmt.Errorf("config: storage.mail_crypt.write: %q is neither off nor age", s.MailCrypt.Write)
	}
	if _, err := resolveSize("storage.mail_crypt.reference_cache_size", s.MailCrypt.ReferenceCacheSize); err != nil {
		return err
	}
	dir := s.MailTempDir
	if dir == "" || !filepath.IsAbs(dir) {
		return fmt.Errorf("config: storage.mail_temp_dir: %q is not an absolute path", dir)
	}
	dir = filepath.Clean(dir)
	// Every location that holds mail or its index; mail_volatile_path is meant
	// to be local and may hold this directory.
	locations := map[string]string{
		"maildir_root": s.MaildirRoot, "mail_home": s.MailHome, "mail_path": s.MailPath,
		"mail_inbox_path": s.MailInboxPath, "mail_index_path": s.MailIndexPath,
		"mail_control_path": s.MailControlPath, "mail_alt_path": s.MailAltPath,
	}
	for name, loc := range locations {
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
