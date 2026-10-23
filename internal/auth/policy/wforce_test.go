//go:build wforce

package policy

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

// The rows run against a real wforce and the sandbox's yarilo with the policy
// overlay on; hack/stand/policy-wforce.sh sets the environment and runs them.
// wforce's policy: a tarpit past 3 different failed passwords per IP+login, a
// refusal with refuseMessage past 50 per IP; every request's fields logged.

const (
	tarpitSecs    = 3
	refuseAfter   = 50
	refuseMessage = "Too many failed logins, try again later"
)

type sandbox struct {
	wforceURL, wforcePass string
	imapAddr, imapName    string
	password              string
	scramUser             string
	kubeconfig            string
}

func sandboxEnv(t *testing.T) sandbox {
	t.Helper()
	sb := sandbox{
		wforceURL:  os.Getenv("WFORCE_URL"),
		wforcePass: os.Getenv("WFORCE_PASSWORD"),
		imapAddr:   os.Getenv("YARILO_IMAP_ADDR"),
		imapName:   os.Getenv("YARILO_IMAP_NAME"),
		password:   os.Getenv("YARILO_PASSWORD"),
		scramUser:  os.Getenv("YARILO_SCRAM_USER"),
		kubeconfig: os.Getenv("KUBECONFIG"),
	}
	if sb.wforceURL == "" || sb.wforcePass == "" || sb.imapAddr == "" || sb.password == "" {
		t.Fatal("WFORCE_URL, WFORCE_PASSWORD, YARILO_IMAP_ADDR and YARILO_PASSWORD are needed; run hack/stand/policy-wforce.sh")
	}
	if sb.scramUser == "" {
		sb.scramUser = "scram@d00001.test"
	}
	return sb
}

// wforceCall posts one command straight to wforce, as a sender would.
func (sb sandbox) wforceCall(t *testing.T, command string, body map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, strings.TrimSuffix(sb.wforceURL, "/")+"/?command="+command, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("wforce", sb.wforcePass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("wforce %s: %v", command, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wforce %s answered %d", command, resp.StatusCode)
	}
	return out
}

// spray reports one failed password from ip for each of n logins, at once.
func (sb sandbox) spray(t *testing.T, ip string, n int) {
	t.Helper()
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { sb.failures(t, fmt.Sprintf("spray%d@test.invalid", i), ip, 1) })
	}
	wg.Wait()
}

func (sb sandbox) reset(t *testing.T, login, ip string) {
	t.Helper()
	sb.wforceCall(t, "reset", map[string]any{"login": login, "ip": ip})
}

// standSeed marks what a row sends wforce itself, apart from a stand login.
const standSeed = "stand-seed"

// failures reports n different failed passwords for login from ip.
func (sb sandbox) failures(t *testing.T, login, ip string, n int) {
	t.Helper()
	for i := range n {
		sb.wforceCall(t, "report", map[string]any{"login": login, "remote": ip,
			"pwhash": fmt.Sprintf("%x", sha256.Sum256([]byte(login+fmt.Sprint(i))))[:3], "success": false, "policy_reject": false, "protocol": "imap", "session_id": standSeed})
	}
}

func (sb sandbox) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{
		URL:              sb.wforceURL,
		APIHeader:        "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("wforce:"+sb.wforcePass)),
		HashMech:         "sha256",
		HashNonce:        "policy-test",
		HashTruncateBits: 12,
		Timeout:          5 * time.Second,
		HTTPClient:       http.DefaultClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// logged is one request as wforce logged it: the command and its fields.
type logged struct {
	command string
	fields  map[string]string
}

var logLine = regexp.MustCompile(`\] (allow|report): (.*)$`)

// logKey finds where a field starts; wforce does not escape quotes in values,
// so a value runs to the quote before the next key.
var logKey = regexp.MustCompile(`(?:^| )([a-z_]+)="`)

func logFields(s string) map[string]string {
	out := map[string]string{}
	keys := logKey.FindAllStringSubmatchIndex(s, -1)
	for i, k := range keys {
		end := len(s)
		if i+1 < len(keys) {
			end = keys[i+1][0]
		}
		out[s[k[2]:k[3]]] = strings.TrimSuffix(strings.TrimRight(s[k[1]:end], " "), `"`)
	}
	return out
}

// requests reads the requests wforce logged for login since a moment.
func (sb sandbox) requests(t *testing.T, login string, since time.Time) []logged {
	t.Helper()
	args := []string{"-n", "wforce", "logs", "deploy/wforce", "--since-time=" + since.Add(-time.Second).UTC().Format(time.RFC3339)}
	if sb.kubeconfig != "" {
		args = append([]string{"--kubeconfig=" + sb.kubeconfig}, args...)
	}
	out, err := exec.Command("kubectl", args...).Output()
	if err != nil {
		t.Fatalf("kubectl logs wforce: %v", err)
	}
	var got []logged
	for _, line := range strings.Split(string(out), "\n") {
		m := logLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		l := logged{command: m[1], fields: logFields(m[2])}
		if l.fields["login"] == login {
			got = append(got, l)
		}
	}
	return got
}

// waitRequests polls the log until want requests for login are there.
func (sb sandbox) waitRequests(t *testing.T, login string, since time.Time, want int) []logged {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := sb.requests(t, login, since)
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func first(reqs []logged, command string) (logged, bool) {
	for _, r := range reqs {
		if r.command == command {
			return r, true
		}
	}
	return logged{}, false
}

// The client against wforce: each row one decision or one field.

func TestWforceDecisions(t *testing.T) {
	sb := sandboxEnv(t)
	c := sb.client(t)
	ctx := context.Background()
	cases := []struct {
		name    string
		ip      string
		prepare func(login, ip string)
		check   func(d Decision) string
	}{
		{"a clean login is allowed", "192.0.2.21", func(string, string) {}, func(d Decision) string {
			if !d.Continue || d.Reject || d.TarpitSecs != 0 {
				return fmt.Sprintf("%+v, want plain continue", d)
			}
			return ""
		}},
		{"three different failed passwords tarpit", "192.0.2.22", func(login, ip string) { sb.failures(t, login, ip, 4) }, func(d Decision) string {
			if !d.Continue || d.TarpitSecs != tarpitSecs {
				return fmt.Sprintf("%+v, want a %ds tarpit", d, tarpitSecs)
			}
			return ""
		}},
		{"past the per-IP threshold the login is refused", "192.0.2.23", func(_, ip string) { sb.spray(t, ip, refuseAfter+5) }, func(d Decision) string {
			if !d.Reject {
				return fmt.Sprintf("%+v, want a refusal", d)
			}
			return ""
		}},
		{"a refusal carries the server's message", "192.0.2.24", func(_, ip string) { sb.spray(t, ip, refuseAfter+5) }, func(d Decision) string {
			if d.Message != refuseMessage {
				return fmt.Sprintf("message %q, want %q", d.Message, refuseMessage)
			}
			return ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			login := "decision@test.invalid"
			sb.reset(t, login, tc.ip)
			t.Cleanup(func() { sb.reset(t, login, tc.ip) })
			tc.prepare(login, tc.ip)
			d, err := c.CheckBefore(ctx, Request{Username: login, Password: "x", RemoteIP: tc.ip, Service: "imap", SessionID: standSeed})
			if err != nil {
				t.Fatal(err)
			}
			if msg := tc.check(d); msg != "" {
				t.Error(msg)
			}
		})
	}
}

func TestWforceReportsReachIt(t *testing.T) {
	sb := sandboxEnv(t)
	c := sb.client(t)
	for _, tc := range []struct {
		name    string
		success bool
		want    string
	}{
		{"after a failure", false, "false"},
		{"after a success", true, "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			login := "report-" + tc.want + "@test.invalid"
			since := time.Now()
			c.ReportAfter(context.Background(), Request{Username: login, Password: "x", RemoteIP: "192.0.2.25", Service: "imap", SessionID: standSeed}, tc.success, false)
			r, ok := first(sb.waitRequests(t, login, since, 1), "report")
			if !ok {
				t.Fatal("wforce logged no report")
			}
			if r.fields["success"] != tc.want {
				t.Errorf("success %q, want %q", r.fields["success"], tc.want)
			}
		})
	}
}

func TestWforceBodyFields(t *testing.T) {
	sb := sandboxEnv(t)
	c := sb.client(t)
	login := "fields@test.invalid"
	req := Request{Username: login, Password: "x", RemoteIP: "192.0.2.26", Service: "imap",
		DeviceID: `"name" "policy-test"`, SessionID: "sess-26", TLS: true, FailType: "credentials"}
	since := time.Now()
	c.ReportAfter(context.Background(), req, false, false)
	r, ok := first(sb.waitRequests(t, login, since, 1), "report")
	if !ok {
		t.Fatal("wforce logged no report")
	}
	for _, tc := range []struct{ field, want string }{
		{"login", login},
		{"remote", "192.0.2.26"},
		{"protocol", "imap"},
		{"device_id", `"name" "policy-test"`},
		{"session_id", "sess-26"},
		{"tls", "true"},
		{"fail_type", "credentials"},
		{"pwhash", c.hashPassword(login, "x")},
	} {
		t.Run(tc.field, func(t *testing.T) {
			if got := r.fields[tc.field]; got != tc.want {
				t.Errorf("%s %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

// Logins through the sandbox's yarilo with the policy overlay: what the
// server puts in the body and what it does with the answer.

type imapConn struct {
	conn net.Conn
	r    *bufio.Reader
	n    int
}

func (sb sandbox) dial(t *testing.T) *imapConn {
	t.Helper()
	name := sb.imapName
	if name == "" {
		name, _, _ = net.SplitHostPort(sb.imapAddr)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", sb.imapAddr, &tls.Config{ServerName: name})
	if err != nil {
		t.Fatalf("dial %s: %v", sb.imapAddr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := &imapConn{conn: conn, r: bufio.NewReader(conn)}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := c.r.ReadString('\n'); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	return c
}

// cmd sends one command and returns its tagged reply line.
func (c *imapConn) cmd(t *testing.T, line string) string {
	t.Helper()
	c.n++
	tag := fmt.Sprintf("a%d", c.n)
	if _, err := fmt.Fprintf(c.conn, "%s %s\r\n", tag, line); err != nil {
		t.Fatal(err)
	}
	for {
		l, err := c.r.ReadString('\n')
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if strings.HasPrefix(l, tag+" ") {
			return strings.TrimSpace(l)
		}
	}
}

func (c *imapConn) login(t *testing.T, user, pass string) string {
	t.Helper()
	return c.cmd(t, fmt.Sprintf("LOGIN %q %q", user, pass))
}

// plain runs AUTHENTICATE PLAIN with an initial response, as mail clients do.
func (c *imapConn) plain(t *testing.T, user, pass string) string {
	t.Helper()
	return c.cmd(t, "AUTHENTICATE PLAIN "+base64.StdEncoding.EncodeToString([]byte("\x00"+user+"\x00"+pass)))
}

// thunderbirdID is the ID Thunderbird sends before it authenticates.
const thunderbirdID = `ID ("name" "Thunderbird" "version" "140.4.0")`

// scram runs AUTHENTICATE SCRAM-SHA-256 and returns the tagged reply.
func (c *imapConn) scram(t *testing.T, user, pass string) string {
	t.Helper()
	c.n++
	tag := fmt.Sprintf("a%d", c.n)
	nonce := "policyTestNonce"
	firstBare := "n=" + user + ",r=" + nonce
	fmt.Fprintf(c.conn, "%s AUTHENTICATE SCRAM-SHA-256 %s\r\n", tag, base64.StdEncoding.EncodeToString([]byte("n,,"+firstBare)))
	line, _ := c.r.ReadString('\n')
	if !strings.HasPrefix(line, "+ ") {
		return strings.TrimSpace(line)
	}
	serverFirst, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(line, "+ ")))
	var combined, saltB64 string
	iter := 0
	for _, a := range strings.Split(string(serverFirst), ",") {
		switch {
		case strings.HasPrefix(a, "r="):
			combined = a[2:]
		case strings.HasPrefix(a, "s="):
			saltB64 = a[2:]
		case strings.HasPrefix(a, "i="):
			fmt.Sscanf(a[2:], "%d", &iter) //nolint:errcheck
		}
	}
	salt, _ := base64.StdEncoding.DecodeString(saltB64)
	withoutProof := "c=" + base64.StdEncoding.EncodeToString([]byte("n,,")) + ",r=" + combined
	authMessage := firstBare + "," + string(serverFirst) + "," + withoutProof
	salted := pbkdf2.Key([]byte(pass), salt, iter, sha256.Size, sha256.New)
	mac := func(key, msg []byte) []byte { h := hmac.New(sha256.New, key); h.Write(msg); return h.Sum(nil) }
	clientKey := mac(salted, []byte("Client Key"))
	stored := sha256.Sum256(clientKey)
	sig := mac(stored[:], []byte(authMessage))
	proof := make([]byte, len(clientKey))
	for i := range proof {
		proof[i] = clientKey[i] ^ sig[i]
	}
	fmt.Fprintf(c.conn, "%s\r\n", base64.StdEncoding.EncodeToString([]byte(withoutProof+",p="+base64.StdEncoding.EncodeToString(proof))))
	for {
		l, err := c.r.ReadString('\n')
		if err != nil {
			t.Fatalf("scram: %v", err)
		}
		if strings.HasPrefix(l, "+ ") {
			fmt.Fprintf(c.conn, "\r\n")
			continue
		}
		if strings.HasPrefix(l, tag+" ") {
			return strings.TrimSpace(l)
		}
	}
}

// ourIP is this client's address as the sandbox reports it to wforce.
func (sb sandbox) ourIP(t *testing.T) string {
	t.Helper()
	user := "u12@d00001.test"
	since := time.Now()
	if r := sb.dial(t).login(t, user, sb.password); !strings.Contains(r, "OK") {
		t.Fatalf("login %s: %s", user, r)
	}
	r, ok := first(sb.waitRequests(t, user, since, 1), "allow")
	if !ok || r.fields["remote"] == "" {
		t.Fatal("wforce logged no allow from the sandbox; is the policy overlay deployed?")
	}
	return r.fields["remote"]
}

func TestSandboxLoginBody(t *testing.T) {
	sb := sandboxEnv(t)
	cases := []struct {
		name   string
		user   string
		run    func(c *imapConn, user string)
		field  string
		want   string
		report bool // read the report, not the allow
	}{
		{"PLAIN login is checked", "u13@d00001.test", func(c *imapConn, u string) { c.login(t, u, sb.password) }, "login", "u13@d00001.test", false},
		{"SCRAM login is checked", sb.scramUser, func(c *imapConn, u string) { c.scram(t, u, sb.password) }, "login", sb.scramUser, false},
		{"SCRAM login is reported", sb.scramUser, func(c *imapConn, u string) { c.scram(t, u, sb.password) }, "success", "true", true},
		{"protocol names the service", "u14@d00001.test", func(c *imapConn, u string) { c.login(t, u, sb.password) }, "protocol", "imap", false},
		{"tls says the client connection is TLS", "u15@d00001.test", func(c *imapConn, u string) { c.login(t, u, sb.password) }, "tls", "true", false},
		{"device_id carries the client's ID", "u16@d00001.test", func(c *imapConn, u string) {
			c.cmd(t, `ID ("name" "policy-test")`)
			c.login(t, u, sb.password)
		}, "device_id", `"name" "policy-test"`, false},
		{"a Thunderbird login carries its ID as device_id", "u22@d00001.test", func(c *imapConn, u string) {
			c.cmd(t, thunderbirdID)
			c.plain(t, u, sb.password)
		}, "device_id", `"name" "Thunderbird" "version" "140.4.0"`, false},
		{"a wrong password reports fail_type credentials", "u17@d00001.test", func(c *imapConn, u string) { c.login(t, u, "not-"+sb.password) }, "fail_type", "credentials", true},
		{"an unknown account reports fail_type account", "nobody-policy@d00001.test", func(c *imapConn, u string) { c.login(t, u, sb.password) }, "fail_type", "account", true},
		{"a success is reported", "u18@d00001.test", func(c *imapConn, u string) { c.login(t, u, sb.password) }, "success", "true", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			since := time.Now()
			tc.run(sb.dial(t), tc.user)
			reqs := sb.waitRequests(t, tc.user, since, 2)
			command := "allow"
			if tc.report {
				command = "report"
			}
			r, ok := first(reqs, command)
			if !ok {
				t.Fatalf("wforce logged no %s for %s", command, tc.user)
			}
			if got := r.fields[tc.field]; got != tc.want {
				t.Errorf("%s %q, want %q", tc.field, got, tc.want)
			}
		})
	}
	t.Run("session_id is set", func(t *testing.T) {
		user := "u19@d00001.test"
		since := time.Now()
		sb.dial(t).login(t, user, sb.password)
		r, ok := first(sb.waitRequests(t, user, since, 1), "allow")
		if !ok {
			t.Fatal("wforce logged no allow")
		}
		if r.fields["session_id"] == "" {
			t.Error("session_id is empty")
		}
	})
}

func TestSandboxPolicyAnswers(t *testing.T) {
	sb := sandboxEnv(t)
	ip := sb.ourIP(t)

	t.Run("a refusal's message reaches the client", func(t *testing.T) {
		user := "u20@d00001.test"
		t.Cleanup(func() { sb.reset(t, user, ip) })
		sb.spray(t, ip, refuseAfter+5)
		r := sb.dial(t).login(t, user, sb.password)
		if !strings.Contains(r, refuseMessage) {
			t.Errorf("reply %q does not carry %q", r, refuseMessage)
		}
	})

	t.Run("a success is not tarpitted after the check", func(t *testing.T) {
		user := "u21@d00001.test"
		sb.reset(t, user, ip)
		t.Cleanup(func() { sb.reset(t, user, ip) })
		sb.failures(t, user, ip, 4)
		c := sb.dial(t)
		start := time.Now()
		r := c.login(t, user, sb.password)
		took := time.Since(start)
		if !strings.Contains(r, "OK") {
			t.Fatalf("login: %s", r)
		}
		// One tarpit before the exchange; a second one after the check
		// doubles it.
		if took >= 2*tarpitSecs*time.Second-500*time.Millisecond {
			t.Errorf("the login took %v: tarpitted after the check as well", took)
		}
		if took < tarpitSecs*time.Second {
			t.Errorf("the login took %v: not tarpitted before the exchange", took)
		}
	})
}
