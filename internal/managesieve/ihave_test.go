package managesieve

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/sieve"
)

// PUTSCRIPT and CHECKSCRIPT under sieve_extensions [ihave]: require "ihave" opens
// nothing outside an ihave block; a block under a refused extension is dead (#2297).
func TestUploadIhaveHoldsToSieveExtensions(t *testing.T) {
	for _, c := range []struct {
		cmd    string
		script string
		wantOK bool
	}{
		{"PUTSCRIPT \"s\"", `require "ihave"; execute "prog";`, false},
		{"PUTSCRIPT \"s\"", `require "ihave"; fileinto "X";`, false},
		{"PUTSCRIPT \"s\"", `require "ihave"; if ihave "fileinto" { fileinto "X"; }`, true},
		{"CHECKSCRIPT", `require "ihave"; execute "prog";`, false},
		{"CHECKSCRIPT", `require "ihave"; fileinto "X";`, false},
		{"CHECKSCRIPT", `require "ihave"; if ihave "fileinto" { fileinto "X"; }`, true},
	} {
		client, server := net.Pipe()
		sess := &session{conn: server, r: bufio.NewReader(server), w: bufio.NewWriter(server),
			username: "u1@example.com", homeDir: t.TempDir(), store: newTestStore(),
			maxSize: 65536, maxLine: 65536, allowedExtensions: sieve.ExtensionSet{List: []string{"ihave"}}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		go sess.serve(ctx)
		tc := &testClient{t: t, conn: client, r: bufio.NewReader(client)}
		tc.readUntilResult()
		tc.send(fmt.Sprintf("%s {%d+}\r\n%s", c.cmd, len(c.script), c.script))
		if lines, ok := tc.readUntilResult(); ok != c.wantOK {
			t.Errorf("%s %q: ok=%v %v, want ok=%v", c.cmd, c.script, ok, lines, c.wantOK)
		}
		tc.close()
		cancel()
	}
}
