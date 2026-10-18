package backend

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/pkg/config"
)

// The sieve section's limits, and the user's extension set, reach the
// ManageSieve server.
func TestManageSieveTakesTheSieveLimits(t *testing.T) {
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
	cfg.Services.ManageSieveBE = &config.ServiceConfig{Enabled: true}
	cfg.Sieve.MaxScriptSize, cfg.Sieve.MaxScripts, cfg.Sieve.MaxStorage = 100, 3, 500
	cfg.Sieve.SieveExtensions, cfg.Sieve.GlobalExtensions = []string{"fileinto"}, []string{"vnd.yarilo.pipe"}

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.managesieve == nil {
		t.Fatal("no ManageSieve server was built")
	}
	opts := reflect.ValueOf(s.managesieve).Elem().FieldByName("opts")
	if got := opts.FieldByName("MaxScripts").Int(); got != 3 {
		t.Errorf("ManageSieve MaxScripts = %d, want sieve_max_num_scripts 3", got)
	}
	if got := opts.FieldByName("MaxStorage").Int(); got != 500 {
		t.Errorf("ManageSieve MaxStorage = %d, want sieve_max_storage 500", got)
	}
	if got := opts.FieldByName("MaxScriptSize").Int(); got != 100 {
		t.Errorf("ManageSieve MaxScriptSize = %d, want sieve_max_script_size 100", got)
	}
	want := sieve.ExtensionSet{List: []string{"fileinto"}, Deny: []string{"vnd.yarilo.pipe"}}
	if got := fmt.Sprint(opts.FieldByName("SieveExtensions")); got != fmt.Sprint(want) {
		t.Errorf("ManageSieve SieveExtensions = %+v, want the user's set %+v", got, want)
	}
}
