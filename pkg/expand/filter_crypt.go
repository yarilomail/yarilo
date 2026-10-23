package expand

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	cryptDefaultAlgo   = "AES-256-CBC"
	cryptDefaultRounds = 10000
	cryptDefaultHash   = "sha256"
	cryptSaltLen       = 8
	cryptSaltChars     = "#&()*+-./0123456789:;<>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[]^_`abcdefghijklmnopqrstuvwxyz{|}"
)

// symCipher is an AES cipher by its OpenSSL name: aes-128/192/256 in CBC or
// ECB mode, PKCS#7 padded.
type symCipher struct {
	keyLen int
	cbc    bool
}

func lookupCipher(name string) (symCipher, error) {
	lower := strings.ToLower(name)
	bits, mode, ok := strings.Cut(strings.TrimPrefix(lower, "aes-"), "-")
	if ok && strings.HasPrefix(lower, "aes-") && (mode == "cbc" || mode == "ecb") {
		switch bits {
		case "128", "192", "256":
			n, _ := strconv.Atoi(bits)
			return symCipher{keyLen: n / 8, cbc: mode == "cbc"}, nil
		}
	}
	return symCipher{}, fmt.Errorf("Invalid cipher %s", name)
}

func (s symCipher) ivLen() int {
	if s.cbc {
		return aes.BlockSize
	}
	return 0
}

func (s symCipher) run(encrypt bool, key, iv, in []byte) ([]byte, error) {
	if len(key) != s.keyLen {
		return nil, fmt.Errorf("Key length invalid (%d != %d)", len(key), s.keyLen)
	}
	if len(iv) != s.ivLen() {
		return nil, fmt.Errorf("IV length invalid (%d != %d)", len(iv), s.ivLen())
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	if encrypt {
		pad := bs - len(in)%bs
		data := append(append([]byte(nil), in...), bytes.Repeat([]byte{byte(pad)}, pad)...)
		out := make([]byte, len(data))
		if s.cbc {
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
		} else {
			for i := 0; i < len(data); i += bs {
				block.Encrypt(out[i:i+bs], data[i:i+bs])
			}
		}
		return out, nil
	}
	if len(in) == 0 || len(in)%bs != 0 {
		return nil, errors.New("Decryption failed: wrong final block length")
	}
	out := make([]byte, len(in))
	if s.cbc {
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, in)
	} else {
		for i := 0; i < len(in); i += bs {
			block.Decrypt(out[i:i+bs], in[i:i+bs])
		}
	}
	pad := int(out[len(out)-1])
	if pad == 0 || pad > bs || !bytes.Equal(out[len(out)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("Decryption failed: bad decrypt")
	}
	return out[:len(out)-pad], nil
}

type cryptSettings struct {
	algo   string
	iv     []byte
	ivSet  bool
	key    []byte
	rounds int64
	salt   *string
	input  []byte
	raw    bool
}

func makeSalt() string {
	b := make([]byte, cryptSaltLen)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = cryptSaltChars[int(b[i])%len(cryptSaltChars)]
	}
	return string(b)
}

// cryptParse reads the decrypt input's own iv or salt and rounds, and its
// ciphertext: "<iv hex or s=salt,r=rounds>$<hex>$".
func (cs *cryptSettings) cryptParse(input string) error {
	parts := strings.Split(input, "$")
	if len(parts) != 3 || parts[2] != "" {
		return errors.New("Invalid input format")
	}
	if cs.ivSet {
		return errors.New("Cannot have iv in parameter and input")
	}
	if p := parts[0]; p != "" && (p[0] == 's' || p[0] == 'r') {
		for _, kv := range strings.Split(p, ",") {
			switch {
			case strings.HasPrefix(kv, "s="):
				s := kv[2:]
				cs.salt = &s
			case strings.HasPrefix(kv, "r="):
				n, err := strconv.ParseInt(kv[2:], 10, 64)
				if err != nil || n < 1 {
					return errors.New("Invalid input")
				}
				cs.rounds = n
			default:
				return errors.New("Invalid input")
			}
		}
	} else {
		iv, err := hex.DecodeString(parts[0])
		if err != nil {
			return errors.New("Invalid iv")
		}
		cs.iv, cs.ivSet = iv, true
	}
	in, err := hex.DecodeString(parts[1])
	if err != nil {
		return errors.New("Invalid input")
	}
	cs.input = in
	return nil
}

func (c *call) cryptSettings(decrypt bool) (*cryptSettings, error) {
	cs := &cryptSettings{algo: cryptDefaultAlgo, rounds: cryptDefaultRounds}
	hashName := cryptDefaultHash
	var keyText *string
	for i := range c.params() {
		p := &c.params()[i]
		switch p.key {
		case "":
			return nil, errTooManyPositional
		case "algorithm":
			v, err := c.str(p)
			if err != nil {
				return nil, err
			}
			cs.algo = v
		case "iv":
			v, err := c.str(p)
			if err != nil {
				return nil, err
			}
			if cs.salt != nil {
				return nil, errors.New("Cannot use both salt and iv")
			}
			iv, err := hex.DecodeString(v)
			if err != nil {
				return nil, errors.New("Invalid iv")
			}
			cs.iv, cs.ivSet = iv, true
		case "key":
			v, err := c.str(p)
			if err != nil {
				return nil, err
			}
			if v == "" {
				return nil, errors.New("Empty encryption key")
			}
			keyText = &v
		case "raw":
			v, err := c.boolean(p)
			if err != nil {
				return nil, err
			}
			cs.raw = v
		case "salt":
			v, err := c.str(p)
			if err != nil {
				return nil, err
			}
			if cs.ivSet {
				return nil, errors.New("Cannot use both salt and iv")
			}
			cs.salt = &v
		case "hash":
			v, err := c.str(p)
			if err != nil {
				return nil, err
			}
			hashName = v
		case "rounds":
			n, err := c.number(p)
			if err != nil {
				return nil, err
			}
			if n < 1 {
				return nil, errors.New("rounds must be positive integer")
			}
			cs.rounds = n
		default:
			return nil, errUnsupportedKey(p.key)
		}
	}
	if keyText == nil {
		return nil, errors.New("Encryption key missing")
	}
	if err := c.needTransfer(c.stmt.function); err != nil {
		return nil, err
	}
	cs.input = append([]byte(nil), c.st.transfer...)
	switch {
	case !cs.raw && decrypt:
		if err := cs.cryptParse(string(c.st.transfer)); err != nil {
			return nil, err
		}
	case cs.raw && !cs.ivSet && cs.salt == nil:
		return nil, errors.New("In raw format, salt or IV must be given")
	}
	if !cs.ivSet {
		if cs.salt == nil {
			s := makeSalt()
			cs.salt = &s
		}
		sym, err := lookupCipher(cs.algo)
		if err != nil {
			return nil, err
		}
		h, ok := hashMethods[hashName]
		if !ok {
			return nil, fmt.Errorf("Unsupported hash %s", hashName)
		}
		material, err := pbkdf2.Key(h, *keyText, []byte(*cs.salt), int(cs.rounds), sym.keyLen+sym.ivLen())
		if err != nil {
			return nil, err
		}
		cs.key, cs.iv = material[:sym.keyLen], material[sym.keyLen:]
		return cs, nil
	}
	key, err := hex.DecodeString(*keyText)
	if err != nil || len(key) == 0 {
		return nil, errors.New("Invalid key")
	}
	cs.key = key
	return cs, nil
}

func fnEncrypt(c *call) error {
	cs, err := c.cryptSettings(false)
	if err != nil {
		return err
	}
	sym, err := lookupCipher(cs.algo)
	if err != nil {
		return err
	}
	if cs.ivSet && len(cs.iv) == 0 && sym.ivLen() > 0 {
		cs.iv = make([]byte, sym.ivLen())
		_, _ = rand.Read(cs.iv)
	}
	out, err := sym.run(true, cs.key, cs.iv, cs.input)
	if err != nil {
		return err
	}
	if cs.raw {
		c.st.setBinary(out)
		return nil
	}
	var b strings.Builder
	if cs.salt != nil {
		fmt.Fprintf(&b, "s=%s,r=%d", *cs.salt, cs.rounds)
	} else {
		b.WriteString(hex.EncodeToString(cs.iv))
	}
	b.WriteString("$" + hex.EncodeToString(out) + "$")
	c.st.set = true
	c.st.transfer = append(c.st.transfer[:0], b.String()...)
	return nil
}

func fnDecrypt(c *call) error {
	cs, err := c.cryptSettings(true)
	if err != nil {
		return err
	}
	sym, err := lookupCipher(cs.algo)
	if err != nil {
		return err
	}
	out, err := sym.run(false, cs.key, cs.iv, cs.input)
	if err != nil {
		return err
	}
	if bytes.IndexByte(out, 0) >= 0 {
		c.st.setBinary(out)
	} else {
		c.st.setString(string(out))
	}
	return nil
}
