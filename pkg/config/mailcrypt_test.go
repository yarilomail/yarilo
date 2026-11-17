package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestCheckMailCrypt(t *testing.T) {
	dir := t.TempDir()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	pub := filepath.Join(dir, "public")
	priv := filepath.Join(dir, "private")
	bad := filepath.Join(dir, "bad")
	for path, data := range map[string]string{pub: id.Recipient().String() + "\n", priv: id.String() + "\n", bad: "not a key\n"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(dir, "missing")

	cases := []struct {
		name string
		set  func(s *StorageConfig)
		want string // "" accepts
	}{
		{"defaults, no Secret mounted", func(s *StorageConfig) {}, ""},
		// A director or a login service loads the same config and holds no
		// keys: the files are read where a store is built, not at load.
		{"age with no key files at all", func(s *StorageConfig) {
			s.MailCrypt.Write, s.MailCrypt.PublicKey, s.MailCrypt.PrivateKeys = "age", missing, missing
			s.MailCrypt.ReferenceKeys = []string{missing}
		}, ""},
		{"age with garbage key files", func(s *StorageConfig) {
			s.MailCrypt.Write, s.MailCrypt.PublicKey, s.MailCrypt.PrivateKeys = "age", bad, bad
		}, ""},
		{"unknown write", func(s *StorageConfig) { s.MailCrypt.Write = "aes" }, "write"},
		{"cache size garbage", func(s *StorageConfig) { s.MailCrypt.ReferenceCacheSize = "lots" }, "reference_cache_size"},
		{"temp dir relative", func(s *StorageConfig) { s.MailTempDir = "tmp" }, "mail_temp_dir"},
		{"temp dir empty", func(s *StorageConfig) { s.MailTempDir = "" }, "mail_temp_dir"},
		{"temp dir under the maildir root", func(s *StorageConfig) { s.MaildirRoot, s.MailTempDir = "/var/mail", "/var/mail/tmp" }, "mail_temp_dir"},
		{"temp dir under the home template", func(s *StorageConfig) { s.MailHome, s.MailTempDir = "/srv/home/%d/%n", "/srv/home/x" }, "mail_temp_dir"},
		{"temp dir beside the root", func(s *StorageConfig) { s.MaildirRoot, s.MailTempDir = "/var/mail", "/var/mailtmp" }, ""},
		{"temp dir under mail_path", func(s *StorageConfig) { s.MailPath, s.MailTempDir = "/data/%d/%n/Maildir", "/data/t" }, "mail_path"},
		{"temp dir under mail_inbox_path", func(s *StorageConfig) { s.MailInboxPath, s.MailTempDir = "/inbox/%u", "/inbox/t" }, "mail_inbox_path"},
		{"temp dir under mail_index_path", func(s *StorageConfig) { s.MailIndexPath, s.MailTempDir = "/idx/%u", "/idx/t" }, "mail_index_path"},
		{"temp dir under mail_control_path", func(s *StorageConfig) { s.MailControlPath, s.MailTempDir = "/ctl/%u", "/ctl/t" }, "mail_control_path"},
		{"temp dir under mail_alt_path", func(s *StorageConfig) { s.MailAltPath, s.MailTempDir = "/alt/%u", "/alt/t" }, "mail_alt_path"},
		{"temp dir under the volatile path", func(s *StorageConfig) { s.MailVolatilePath, s.MailTempDir = "/run/yarilo", "/run/yarilo/t" }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Defaults().Storage
			s.MaildirRoot = "/var/mail/vhosts"
			s.MailHome = "%d/%u"
			c.set(&s)
			err := checkMailCrypt(s)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("err %v, want one naming %s", err, c.want)
			}
		})
	}
}

// A config written for schema 31 names neither key: it loads with writing
// off and the default temp directory, as it ran before.
func TestSchema31ConfigLoadsWithMailCryptOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(path, []byte("config_schema_version: 31\nstorage:\n  mail_driver: maildir\n  maildir_root: /var/mail/vhosts\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.Storage.MailCrypt
	if mc.Write != "off" || mc.ReferenceCacheSize != "256M" || cfg.Storage.MailTempDir != "/tmp/yarilo-mail" {
		t.Fatalf("defaults not applied: %+v, temp %q", mc, cfg.Storage.MailTempDir)
	}
}
