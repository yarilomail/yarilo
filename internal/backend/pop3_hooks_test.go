package backend

import (
	"context"
	"net"
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	_ "github.com/yarilomail/yarilo/v2/pkg/dict/file"
)

// POP3 gets the mutation layer's hooks the process built, as IMAP and LMTP
// do: without them a POP3 deletion stays in FTS and moves no quota (#2213).
func TestPOP3IsWiredWithTheMutationHooks(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.MaildirRoot = t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go protocol.NewServer(nil).Serve(ctx, ln)  //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close() }) //nolint:errcheck
	cfg.AuthService.Addr = ln.Addr().String()
	cfg.Services.POP3 = &config.ServiceConfig{Enabled: true}
	cfg.Quota.Enabled = true
	cfg.Quota.WarningBinDir = t.TempDir()
	cfg.Quota.CloneDicts = []string{"quota"}
	cfg.Dicts = map[string]config.DictConfig{
		"quota": {Driver: "file", Settings: map[string]any{"path": "%h/yarilo-quota.json"}},
	}
	cfg.DictService.DictAddr = "127.0.0.1:1" // a file dict opens here, never dialled
	cfg.FTS.Enabled, cfg.FTS.Mode, cfg.FTS.Addr = true, "remote", "127.0.0.1:1"

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.pop3 == nil {
		t.Fatal("no POP3 server was built")
	}
	opts := reflect.ValueOf(s.pop3).Elem().FieldByName("opts")
	for _, field := range []string{"FTSClient", "QuotaWarner", "QuotaClone"} {
		if opts.FieldByName(field).IsNil() {
			t.Errorf("POP3 was built without %s", field)
		}
	}
	if !opts.FieldByName("QuotaEngine").Bool() {
		t.Error("POP3 was built with the quota engine off while quota is enabled")
	}
}
