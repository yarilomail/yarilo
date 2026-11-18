package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
)

// captureStd returns what fn wrote to stdout and stderr.
func captureStd(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	runErr := fn()
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String(), runErr
}

func TestKeygenKeepsThePrivateKeyInTheFile(t *testing.T) {
	for _, typ := range []string{"x25519", "hybrid"} {
		t.Run(typ, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			out, err := captureStd(t, func() error { return cryptKeygen([]string{"--type", typ, "--out", path}) })
			if err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != 0o600 {
				t.Fatalf("mode %o", st.Mode().Perm())
			}
			data, _ := os.ReadFile(path)
			ids, err := age.ParseIdentities(bytes.NewReader(data))
			if err != nil || len(ids) != 1 {
				t.Fatalf("the file holds no identity: %v", err)
			}
			if strings.Contains(out, "AGE-SECRET-KEY") {
				t.Fatal("the private key reached the output")
			}
			if !strings.HasPrefix(strings.TrimSpace(out), "age1") || strings.Count(strings.TrimSpace(out), "\n") != 0 {
				t.Fatalf("output %q, want the public key alone", out)
			}

			before, _ := os.ReadFile(path)
			if _, err := captureStd(t, func() error { return cryptKeygen([]string{"--type", typ, "--out", path}) }); err == nil {
				t.Fatal("an existing key file was not refused")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("the existing key file was changed")
			}
		})
	}
}

func TestKeygenRefusesAnUnknownType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if _, err := captureStd(t, func() error { return cryptKeygen([]string{"--type", "rsa", "--out", path}) }); err == nil {
		t.Fatal("rsa accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a file was written for a refused type")
	}
}

// --all runs every user, each job to its end, sums the states, names a user
// whose job failed, runs the public namespace once, and exits non-zero.
func TestCryptAllSumsEveryUser(t *testing.T) {
	var mu sync.Mutex
	started := map[string]map[string]any{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/backend/user/iterate":
			_, _ = io.WriteString(w, `{"users":["a@x","b@x","c@x"]}`)
		case r.URL.Path == "/api/backend/crypt/status":
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			key := req["user"].(string)
			if ns, _ := req["namespace"].(string); ns != "" {
				key = "ns:" + ns
			}
			mu.Lock()
			started[key] = req
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Path == "/api/backend/crypt/job":
			u := r.URL.Query().Get("user")
			mu.Lock()
			ns, _ := started["ns:public"]["user"].(string)
			public := ns == u && len(started) == 4
			mu.Unlock()
			switch {
			case public:
				_, _ = io.WriteString(w, `{"kind":"status","user":"`+u+`","state":"done","totals":{"plain":5}}`)
			case u == "b@x":
				_, _ = io.WriteString(w, `{"kind":"status","user":"b@x","state":"done","totals":{"current":1},"folders_failed":1}`)
			default:
				_, _ = io.WriteString(w, `{"kind":"status","user":"`+u+`","state":"done","totals":{"current":2,"other_key":1}}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	oldRoute, oldBackend := routeByUser, backendAPIURL
	defer func() { routeByUser, backendAPIURL = oldRoute, oldBackend }()
	routeByUser, backendAPIURL = false, ts.URL
	oldFmt := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = oldFmt }()

	out, err := captureStd(t, func() error { return cryptAll("status", map[string]any{}, 2, 0, "public") })
	if err == nil || !strings.Contains(err.Error(), "b@x") {
		t.Fatalf("err %v, want the failed user named", err)
	}
	if len(started) != 4 || started["ns:public"] == nil {
		t.Fatalf("started %v, want three users and the public namespace", started)
	}
	i := strings.Index(out, "{\n")
	if i < 0 {
		t.Fatalf("no summary in %q", out)
	}
	var sum allSummary
	if err := json.Unmarshal([]byte(out[i:strings.LastIndex(out, "}")+1]), &sum); err != nil {
		t.Fatalf("summary %q: %v", out[i:], err)
	}
	if sum.Users != 3 || sum.Totals["current"] != 5 || sum.Totals["other_key"] != 2 || sum.Totals["plain"] != 5 || sum.FoldersFailed != 1 {
		t.Fatalf("summary %+v", sum)
	}
}

// The rate is shared between the jobs running at once.
func TestCryptAllSharesTheRate(t *testing.T) {
	var mu sync.Mutex
	rates := []float64{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/backend/user/iterate":
			_, _ = io.WriteString(w, `{"users":["a@x","b@x"]}`)
		case "/api/backend/crypt/reencrypt":
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			rates = append(rates, req["rate"].(float64))
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{}`)
		case "/api/backend/crypt/job":
			_, _ = io.WriteString(w, `{"kind":"reencrypt","state":"done","totals":{}}`)
		}
	}))
	defer ts.Close()
	oldRoute, oldBackend := routeByUser, backendAPIURL
	defer func() { routeByUser, backendAPIURL = oldRoute, oldBackend }()
	routeByUser, backendAPIURL = false, ts.URL
	if _, err := captureStd(t, func() error { return cryptAll("reencrypt", map[string]any{}, 2, 100, "") }); err != nil {
		t.Fatal(err)
	}
	if len(rates) != 2 || rates[0] != 50 || rates[1] != 50 {
		t.Fatalf("rates %v, want 50 each", rates)
	}
}
