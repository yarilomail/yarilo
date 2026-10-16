// Package stopaccept serves a backend listener until shutdown and then closes
// it, so a stopping backend refuses new sessions; open ones end with the process.
package stopaccept

import (
	"context"
	"log/slog"
	"net"
	"os"
)

// Serve runs serve(ln) until ctx ends, then closes ln so a new session is refused
// and rerouted (#2260); serve ending before that is fatal.
func Serve(ctx context.Context, name string, ln net.Listener, serve func(net.Listener) error) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	go func() {
		err := serve(ln)
		if ctx.Err() != nil {
			return
		}
		slog.Error(name+" server error", "err", err)
		exit(1)
	}()
}

// exit is os.Exit; a test stands in for it.
var exit = os.Exit
