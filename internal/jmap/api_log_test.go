package jmap

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// secretArg stands for what a request carries and its log line must not.
const secretArg = "secret-argument-value"

// At debug a request is logged by path, capabilities and method names, a
// refused one too, and never with its arguments.
func TestADebugLineNamesTheRequestNotItsArguments(t *testing.T) {
	core := `"urn:ietf:params:jmap:core"`
	for _, tc := range []struct {
		name, body, msg string
		level           slog.Level
		using, methods  []string
	}{
		{"an accepted request",
			`{"using":[` + core + `],"methodCalls":[["Core/echo",{"x":"` + secretArg + `"},"c0"],["Core/echo",{},"c1"]]}`,
			"jmap: request", slog.LevelDebug,
			[]string{"urn:ietf:params:jmap:core"}, []string{"Core/echo", "Core/echo"}},
		{"an unknown capability",
			`{"using":[` + core + `,"urn:ietf:params:jmap:contacts"],"methodCalls":[["AddressBook/get",{"accountId":"` + secretArg + `"},"c0"]]}`,
			"jmap: request rejected", slog.LevelDebug,
			[]string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:contacts"}, []string{"AddressBook/get"}},
		{"above debug, nothing", `{"using":[` + core + `],"methodCalls":[["Core/echo",{},"c0"]]}`,
			"", slog.LevelInfo, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: tc.level})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			trustedServer(t).Handler().ServeHTTP(httptest.NewRecorder(), apiRequest(tc.body))

			if strings.Contains(buf.String(), secretArg) {
				t.Fatalf("the log carries an argument:\n%s", buf.String())
			}
			var line struct {
				Msg     string `json:"msg"`
				Path    string `json:"path"`
				Request struct {
					Using   []string `json:"using"`
					Methods []string `json:"methods"`
				} `json:"request"`
			}
			for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				var probe struct{ Msg string }
				if json.Unmarshal([]byte(l), &probe) == nil && strings.HasPrefix(probe.Msg, "jmap: request") {
					_ = json.Unmarshal([]byte(l), &line)
				}
			}
			if line.Msg != tc.msg {
				t.Fatalf("line %q, want %q in:\n%s", line.Msg, tc.msg, buf.String())
			}
			if tc.msg == "" {
				return
			}
			if line.Path != apiPath || !slices.Equal(line.Request.Using, tc.using) || !slices.Equal(line.Request.Methods, tc.methods) {
				t.Errorf("path %q request %+v, want %q %v %v", line.Path, line.Request, apiPath, tc.using, tc.methods)
			}
		})
	}
}

// A request over a WebSocket is logged under the WebSocket's path.
func TestADebugLineNamesTheWebSocketPath(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	f := pushServer(t, hour, 0)
	c := f.mustWS(t)
	wsSend(t, c, echoRequest)
	if _, ok := wsNext(t, c, "Response", time.Second); !ok {
		t.Fatal("no Response")
	}
	if !strings.Contains(buf.String(), `"msg":"jmap: request","user":"`+testUser+`","session":"deadbeefdeadbeef","path":"/jmap/ws/"`) {
		t.Errorf("no request line with the WebSocket path in:\n%s", buf.String())
	}
}

// syncBuffer is a log sink the server's goroutines write to concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
