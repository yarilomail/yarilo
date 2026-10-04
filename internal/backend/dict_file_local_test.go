package backend

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	_ "github.com/yarilomail/yarilo/pkg/dict/file"
)

// countingListener counts what reaches the dict service. A session that opens
// its own file must not dial it at all (#1733).
type countingListener struct {
	net.Listener
	accepts atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return c, err
}

// The annotation dict named "file" is opened here, and the dict service is
// never dialled: the two round trips an annotation used to cost are gone.
func TestAFileDictIsOpenedWithoutTheDictService(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingListener{Listener: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := counted.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	home := t.TempDir()
	cfg := &config.Config{
		Dicts: map[string]config.DictConfig{
			"metadata": {Driver: "file", Settings: map[string]any{"path": "%h/yarilo-metadata.json"}},
		},
	}
	cfg.DictService.DictAddr = counted.Addr().String()

	d, err := buildDict(cfg, "metadata")
	if err != nil {
		t.Fatalf("buildDict: %v", err)
	}
	if d == nil {
		t.Fatal("the metadata dict is declared and buildDict returned nothing")
	}
	defer d.Close() //nolint:errcheck

	ops := &dict.OpSettings{Username: "u1@d00001.test", HomeDir: home}
	ctx := context.Background()
	// What an APPEND with imapsieve on does: read the annotation, then write one.
	if _, _, err := d.Lookup(ctx, ops, "/shared/imapsieve/script"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	tx, err := d.Begin(ctx, ops)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := tx.Set("/private/vendor/test", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if n := counted.accepts.Load(); n != 0 {
		t.Errorf("the dict service was dialled %d times for a file dict", n)
	}
	if _, err := os.Stat(filepath.Join(home, "yarilo-metadata.json")); err != nil {
		t.Errorf("the annotation did not land in the user's home: %v", err)
	}
}

// A driver that needs an engine still goes through the service, and without an
// address it is an error rather than a session opening a database itself.
func TestAnEngineDictStillNeedsTheService(t *testing.T) {
	cfg := &config.Config{
		Dicts: map[string]config.DictConfig{
			"metadata": {Driver: "redis", Settings: map[string]any{"addr": "127.0.0.1:1"}},
		},
	}
	if _, err := buildDict(cfg, "metadata"); err == nil {
		t.Fatal("a redis dict with no dict_addr was opened")
	}
}
