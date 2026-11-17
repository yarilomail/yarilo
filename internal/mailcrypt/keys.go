package mailcrypt

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"filippo.io/age"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt/refstream"
)

// KeyFiles names the key material of storage.mail_crypt.
type KeyFiles struct {
	Write         string   // "off" (or empty, a config never loaded) or "age"
	PublicKey     string   // age recipient(s), read only when Write is "age"
	PrivateKeys   string   // age identities, read when the file exists
	ReferenceKeys []string // PEM keys of the reference format, each required
}

// LoadKeys reads the key files into Options; the loader and the store builder
// call it alike, so a file that fails here fails the start.
func LoadKeys(k KeyFiles) (Options, error) {
	var o Options
	switch k.Write {
	case "off", "":
	case "age":
		f, err := os.Open(k.PublicKey)
		if err != nil {
			return o, fmt.Errorf("public_key: %w", err)
		}
		o.Recipients, err = age.ParseRecipients(f)
		_ = f.Close()
		if err != nil {
			return o, fmt.Errorf("public_key %s: %w", k.PublicKey, err)
		}
	default:
		return o, fmt.Errorf("write: %q is neither off nor age", k.Write)
	}
	if k.PrivateKeys != "" {
		f, err := os.Open(k.PrivateKeys)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return o, fmt.Errorf("private_keys: %w", err)
		default:
			o.Identities, err = age.ParseIdentities(f)
			_ = f.Close()
			if err != nil {
				return o, fmt.Errorf("private_keys %s: %w", k.PrivateKeys, err)
			}
		}
	}
	for _, p := range k.ReferenceKeys {
		data, err := os.ReadFile(p)
		if err != nil {
			return o, fmt.Errorf("reference_keys: %w", err)
		}
		key, err := refstream.ParsePrivateKeyPEM(data)
		if err != nil {
			return o, fmt.Errorf("reference_keys %s: %w", p, err)
		}
		o.ReferenceKeys = append(o.ReferenceKeys, key)
	}
	return o, nil
}

// Under reports whether dir lies in or below root; both are cleaned absolute
// paths, root may end where a template variable begins.
func Under(dir, root string) bool {
	root = strings.TrimRight(root, "/")
	if root == "" {
		return false
	}
	return dir == root || strings.HasPrefix(dir, root+"/")
}
