package mailboxbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// A process that builds a store stops at start when its keys cannot be read,
// or when age would refuse the recipient set at the first save.
func TestCryptLayerStopsTheStartOnBadKeys(t *testing.T) {
	dir := t.TempDir()
	x, _ := age.GenerateX25519Identity()
	h, _ := age.GenerateHybridIdentity()
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("public", x.Recipient().String()+"\n")
	mixed := write("mixed", x.Recipient().String()+"\n"+h.Recipient().String()+"\n")
	bad := write("bad", "not a key\n")
	missing := filepath.Join(dir, "missing")

	cases := []struct {
		name string
		mc   config.MailCryptConfig
		want string // "" starts
	}{
		{"off, no Secret", config.MailCryptConfig{Write: "off", PublicKey: missing, PrivateKeys: missing}, ""},
		{"age with a key", config.MailCryptConfig{Write: "age", PublicKey: good, PrivateKeys: missing}, ""},
		{"age without the key", config.MailCryptConfig{Write: "age", PublicKey: missing}, "public_key"},
		{"age with garbage", config.MailCryptConfig{Write: "age", PublicKey: bad}, "public_key"},
		{"hybrid beside classic", config.MailCryptConfig{Write: "age", PublicKey: mixed}, "public_key"},
		{"private keys garbage", config.MailCryptConfig{Write: "off", PrivateKeys: bad}, "private_keys"},
		{"reference key missing", config.MailCryptConfig{Write: "off", ReferenceKeys: []string{missing}}, "reference_keys"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stopped error
			old := exitProcess
			exitProcess = func(err error) { stopped = err }
			defer func() { exitProcess = old }()
			sc := config.Defaults().Storage
			sc.MailCrypt = c.mc
			sc.MailTempDir = filepath.Join(t.TempDir(), c.name)
			CryptLayer(sc)
			switch {
			case c.want == "" && stopped != nil:
				t.Fatalf("stopped: %v", stopped)
			case c.want != "" && (stopped == nil || !strings.Contains(stopped.Error(), c.want)):
				t.Fatalf("stopped with %v, want one naming %s", stopped, c.want)
			}
		})
	}
}
