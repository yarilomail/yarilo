package protocol

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SCRAM-SHA-1 is a mechanism this server serves
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"golang.org/x/crypto/pbkdf2"
)

// relaySession is a session as it will be after the cut: it holds the client's
// SASL bytes and the channel binding, and knows no verifier.
type relaySession struct {
	t    *testing.T
	conn net.Conn
	sc   *bufio.Scanner
	id   string
}

func newRelaySession(t *testing.T, srv *Server) *relaySession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)
	conn, sc := dialAndHandshake(t, addr)
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck
	return &relaySession{t: t, conn: conn, sc: sc, id: "1"}
}

// begin sends the client-first message the way a session relays it.
func (r *relaySession) begin(mech string, first []byte, cbind []byte) string {
	r.t.Helper()
	line := fmt.Sprintf("AUTH\t%s\t%s\tservice=imap\tsession=s1\tresp=%s",
		r.id, mech, base64.StdEncoding.EncodeToString(first))
	if len(cbind) > 0 {
		line += "\tcbind=" + base64.StdEncoding.EncodeToString(cbind)
	}
	fmt.Fprintln(r.conn, line)
	return r.read()
}

func (r *relaySession) cont(response []byte) string {
	r.t.Helper()
	fmt.Fprintf(r.conn, "CONT\t%s\t%s\n", r.id, base64.StdEncoding.EncodeToString(response))
	return r.read()
}

func (r *relaySession) read() string {
	r.t.Helper()
	if !r.sc.Scan() {
		r.t.Fatalf("no reply: %v", r.sc.Err())
	}
	return r.sc.Text()
}

// challengeOf takes the server's bytes out of a CONT line.
func challengeOf(t *testing.T, line string) []byte {
	t.Helper()
	fields := strings.Split(line, "\t")
	if len(fields) < 3 || fields[0] != "CONT" {
		t.Fatalf("not a continuation: %q", line)
	}
	raw, err := base64.StdEncoding.DecodeString(fields[2])
	if err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	return raw
}

// scramClientDriver is a minimal SCRAM client, kept here for the same reason
// the go-sasl tests keep one: the wire flow stays visible in the row.
type scramClientDriver struct {
	username, password string
	nonce              string
	gs2                string
	cbData             []byte
	newHash            func() hash.Hash
	hashSize           int
	first              string
}

func newScramClientDriver(mech, user, pass string, cbData []byte) *scramClientDriver {
	d := &scramClientDriver{username: user, password: pass, nonce: "rOprNGfwEbeRWgbNEkqO", cbData: cbData}
	switch mech {
	case MechScramSha256, MechScramSha256Plus:
		d.newHash, d.hashSize = sha256.New, sha256.Size
	case MechScramSha1, MechScramSha1Plus:
		d.newHash, d.hashSize = sha1.New, sha1.Size
	}
	switch mech {
	case MechScramSha256Plus, MechScramSha1Plus:
		// Both PLUS variants bind to the exporter output (RFC 9266): the server
		// refuses any other flag, and tls-unique is not a thing it offers.
		d.gs2 = "p=tls-exporter,,"
	default:
		d.gs2 = "n,,"
	}
	return d
}

func (d *scramClientDriver) clientFirst() []byte {
	d.first = fmt.Sprintf("n=%s,r=%s", d.username, d.nonce)
	return []byte(d.gs2 + d.first)
}

func (d *scramClientDriver) clientFinal(t *testing.T, serverFirst []byte) []byte {
	t.Helper()
	var combinedNonce, saltB64 string
	var iter int
	for _, attr := range strings.Split(string(serverFirst), ",") {
		if len(attr) < 2 {
			continue
		}
		switch attr[0] {
		case 'r':
			combinedNonce = attr[2:]
		case 's':
			saltB64 = attr[2:]
		case 'i':
			fmt.Sscanf(attr[2:], "%d", &iter) //nolint:errcheck
		}
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		t.Fatalf("salt: %v", err)
	}
	cb := append([]byte(d.gs2), d.cbData...)
	withoutProof := fmt.Sprintf("c=%s,r=%s", base64.StdEncoding.EncodeToString(cb), combinedNonce)
	authMessage := d.first + "," + string(serverFirst) + "," + withoutProof

	salted := pbkdf2.Key([]byte(d.password), salt, iter, d.hashSize, d.newHash)
	clientKey := hmacSum(d.newHash, salted, []byte("Client Key"))
	h := d.newHash()
	h.Write(clientKey) //nolint:errcheck
	storedKey := h.Sum(nil)
	clientSig := hmacSum(d.newHash, storedKey, []byte(authMessage))
	proof := make([]byte, len(clientKey))
	for i := range proof {
		proof[i] = clientKey[i] ^ clientSig[i]
	}
	return []byte(fmt.Sprintf("%s,p=%s", withoutProof, base64.StdEncoding.EncodeToString(proof)))
}

func hmacSum(newHash func() hash.Hash, key, data []byte) []byte {
	m := hmac.New(newHash, key)
	m.Write(data) //nolint:errcheck
	return m.Sum(nil)
}

// runSCRAM drives the client through the service. The session never sees a
// verifier: it carries bytes, which is the whole point of the move (#1733).
func runSCRAM(t *testing.T, srv *Server, mech, user, pass string, cbind []byte) string {
	t.Helper()
	d := newScramClientDriver(mech, user, pass, cbind)
	r := newRelaySession(t, srv)
	line := r.begin(mech, d.clientFirst(), cbind)
	if !strings.HasPrefix(line, "CONT\t") {
		return line
	}
	line = r.cont(d.clientFinal(t, challengeOf(t, line)))
	return line
}

func scramChain(t *testing.T, user, pass string) []Passdb {
	t.Helper()
	creds256, err := sasl.GenerateScramSha256Credentials(pass, sasl.MinScramIterations)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	creds1, err := sasl.GenerateScramSha1Credentials(pass, sasl.MinScramIterations)
	if err != nil {
		t.Fatalf("credentials sha1: %v", err)
	}
	return []Passdb{
		&scramPassdb{username: user, creds: creds256},
		&scramSha1Passdb{username: user, creds: creds1},
	}
}

// Every mechanism the service advertises completes through the wire, with the
// session relaying bytes it cannot interpret.
func TestEveryScramMechanismRunsInTheService(t *testing.T) {
	cbind := []byte("tls-exporter-bytes-32-characters")
	rows := []struct {
		mech  string
		cbind []byte
	}{
		{MechScramSha256, nil},
		{MechScramSha256Plus, cbind},
		{MechScramSha1, nil},
		{MechScramSha1Plus, cbind},
	}
	for _, row := range rows {
		t.Run(row.mech, func(t *testing.T) {
			srv := NewServer(scramChain(t, "alice", "hunter2"))
			line := runSCRAM(t, srv, row.mech, "alice", "hunter2", row.cbind)
			if !strings.HasPrefix(line, "OK\t") {
				t.Fatalf("exchange ended %q, want OK", line)
			}
			if !strings.Contains(line, "user=alice") {
				t.Errorf("reply %q does not name the user", line)
			}
			if !strings.Contains(line, "resp=") {
				t.Errorf("reply %q carries no server-final message", line)
			}
		})
	}
}

// A wrong password fails through the same path, and the reply says nothing a
// probe could tell apart from an unknown user.
func TestAWrongPasswordFailsTheExchange(t *testing.T) {
	srv := NewServer(scramChain(t, "alice", "hunter2"))
	line := runSCRAM(t, srv, MechScramSha256, "alice", "wrong", nil)
	if !strings.HasPrefix(line, "FAIL\t") {
		t.Fatalf("exchange ended %q, want FAIL", line)
	}

	unknown := runSCRAM(t, srv, MechScramSha256, "nobody", "hunter2", nil)
	if !strings.HasPrefix(unknown, "FAIL\t") {
		t.Fatalf("unknown user ended %q, want FAIL", unknown)
	}
	if unknown != line {
		t.Errorf("unknown user answered %q and wrong password %q; the two must read alike", unknown, line)
	}
}

// The -PLUS mechanisms bind to the bytes the session holds: a client binding to
// different bytes is refused, which is what channel binding is for.
func TestChannelBindingIsCheckedAgainstTheSessionBytes(t *testing.T) {
	srv := NewServer(scramChain(t, "alice", "hunter2"))
	d := newScramClientDriver(MechScramSha256Plus, "alice", "hunter2", []byte("bytes-the-client-thinks-it-has"))
	r := newRelaySession(t, srv)
	line := r.begin(MechScramSha256Plus, d.clientFirst(), []byte("bytes-the-session-really-holds"))
	if strings.HasPrefix(line, "CONT\t") {
		line = r.cont(d.clientFinal(t, challengeOf(t, line)))
	}
	if !strings.HasPrefix(line, "FAIL\t") {
		t.Fatalf("a mismatched binding ended %q, want FAIL", line)
	}
}

// -PLUS without the bytes is refused rather than run unbound: an exchange that
// silently drops the binding is worse than one that does not start.
func TestPlusWithoutBindingIsRefused(t *testing.T) {
	srv := NewServer(scramChain(t, "alice", "hunter2"))
	r := newRelaySession(t, srv)
	line := r.begin(MechScramSha256Plus, []byte("n,,n=alice,r=rOprNGfwEbeRWgbNEkqO"), nil)
	if !strings.HasPrefix(line, "FAIL\t") {
		t.Fatalf("answer %q, want FAIL", line)
	}
	if !strings.Contains(line, "channel binding") {
		t.Errorf("reply %q does not say what is missing", line)
	}
}

// The mechanism list a session advertises comes from the service, and names
// only what the chain can actually serve.
func TestTheServiceAdvertisesWhatTheChainCanServe(t *testing.T) {
	creds, err := sasl.GenerateScramSha256Credentials("hunter2", sasl.MinScramIterations)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	rows := []struct {
		name  string
		chain []Passdb
		want  []string
		gone  []string
	}{
		{"sha256 only", []Passdb{&scramPassdb{username: "alice", creds: creds}},
			[]string{"SCRAM-SHA-256", "SCRAM-SHA-256-PLUS"}, []string{"SCRAM-SHA-1"}},
		{"no scram at all", []Passdb{&stubPassdb{result: ResultNext}},
			[]string{"PLAIN", "LOGIN"}, []string{"SCRAM-SHA-256", "SCRAM-SHA-1"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			srv := NewServer(row.chain)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			addr := freeAddr(t)
			go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
			time.Sleep(20 * time.Millisecond)
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close() //nolint:errcheck
			sc := bufio.NewScanner(conn)
			var mechs []string
			for sc.Scan() {
				line := sc.Text()
				if strings.HasPrefix(line, "MECH\t") {
					mechs = append(mechs, strings.Split(line, "\t")[1])
				}
				if line == "DONE" {
					break
				}
			}
			joined := strings.Join(mechs, ",")
			for _, want := range row.want {
				if !strings.Contains(joined, want) {
					t.Errorf("handshake advertises %q, missing %s", joined, want)
				}
			}
			for _, gone := range row.gone {
				if strings.Contains(joined, gone) {
					t.Errorf("handshake advertises %s, which no chain entry can serve: %q", gone, joined)
				}
			}
		})
	}
}

// saltOf takes the s= attribute out of a server-first message.
func saltOf(t *testing.T, challenge []byte) string {
	t.Helper()
	for _, attr := range strings.Split(string(challenge), ",") {
		if strings.HasPrefix(attr, "s=") {
			return attr[2:]
		}
	}
	t.Fatalf("no salt in %q", challenge)
	return ""
}

// The first step must not answer "this user exists". A real account always
// yields the same salt, so a fabricated one that changes per attempt is the
// difference a prober reads -- before any password is sent.
func TestTheFirstStepDoesNotSayWhetherTheUserExists(t *testing.T) {
	srv := NewServer(scramChain(t, "alice", "hunter2"))

	saltFor := func(user string) string {
		d := newScramClientDriver(MechScramSha256, user, "whatever", nil)
		r := newRelaySession(t, srv)
		line := r.begin(MechScramSha256, d.clientFirst(), nil)
		return saltOf(t, challengeOf(t, line))
	}

	if a, b := saltFor("alice"), saltFor("alice"); a != b {
		t.Errorf("a real account answered two salts, %q and %q", a, b)
	}
	first, second := saltFor("nobody"), saltFor("nobody")
	if first != second {
		t.Errorf("the same unknown name answered two salts, %q and %q: existence is readable from step one", first, second)
	}
	if other := saltFor("nobody-else"); other == first {
		t.Errorf("two different unknown names answered one salt %q; the fabrication must depend on the name", other)
	}
}

// N logins cost N AUTH commands. The mechanism list travels in the handshake,
// so a session that reads it per login would double the service's traffic.
func TestTheMechanismListCostsNothingPerLogin(t *testing.T) {
	srv := NewServer(scramChain(t, "alice", "hunter2"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	go srv.ListenAndServe(ctx, addr, nil) //nolint:errcheck
	time.Sleep(20 * time.Millisecond)

	conn, sc := dialAndHandshake(t, addr)
	defer conn.Close() //nolint:errcheck

	// One connection, several logins: the handshake happened once, before any
	// of them, and nothing re-reads it.
	const logins = 5
	sent := 0
	for i := 0; i < logins; i++ {
		id := fmt.Sprintf("%d", i+1)
		d := newScramClientDriver(MechScramSha256, "alice", "hunter2", nil)
		fmt.Fprintf(conn, "AUTH\t%s\t%s\tservice=imap\tresp=%s\n", id, MechScramSha256,
			base64.StdEncoding.EncodeToString(d.clientFirst()))
		sent++
		if !sc.Scan() {
			t.Fatalf("no reply: %v", sc.Err())
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "CONT\t") {
			t.Fatalf("login %d answered %q, want a challenge", i, line)
		}
		fmt.Fprintf(conn, "CONT\t%s\t%s\n", id,
			base64.StdEncoding.EncodeToString(d.clientFinal(t, challengeOf(t, line))))
		sent++
		if !sc.Scan() {
			t.Fatalf("no verdict: %v", sc.Err())
		}
		if !strings.HasPrefix(sc.Text(), "OK\t") {
			t.Fatalf("login %d ended %q", i, sc.Text())
		}
	}
	// Two commands per login is the SCRAM round trip itself: AUTH and CONT.
	// A third would be the list being read again.
	if want := logins * 2; sent != want {
		t.Errorf("%d logins cost %d commands, want %d", logins, sent, want)
	}
}

// A SCRAM client may put an authzid in its gs2 header. Master-user
// impersonation is a decision this service makes, and today it makes it only
// for PLAIN: the row records that a SCRAM authzid grants nothing.
func TestAScramAuthzidDoesNotImpersonate(t *testing.T) {
	srv := NewServer(scramChain(t, "master", "hunter2"))
	d := newScramClientDriver(MechScramSha256, "master", "hunter2", nil)
	d.gs2 = "n,a=victim,"

	r := newRelaySession(t, srv)
	line := r.begin(MechScramSha256, d.clientFirst(), nil)
	if strings.HasPrefix(line, "CONT\t") {
		line = r.cont(d.clientFinal(t, challengeOf(t, line)))
	}
	if strings.HasPrefix(line, "OK\t") && strings.Contains(line, "user=victim") {
		t.Fatalf("a SCRAM authzid impersonated another account: %q", line)
	}
	if strings.HasPrefix(line, "OK\t") && !strings.Contains(line, "user=master") {
		t.Errorf("reply %q names neither the authenticating user nor a refusal", line)
	}
}

// A conversation nobody finishes must not be kept: the relay's connection is
// one per mail process, so what is held on it is held for that process's life.
func TestAnAbandonedExchangeIsDropped(t *testing.T) {
	live := newExchanges()
	clock := time.Now()
	live.now = func() time.Time { return clock }

	// Put and walk away: nothing touches it again, which is the case the
	// deadline exists for. Taking it first would reset that deadline.
	live.put("1", &saslExchange{mech: MechScramSha256})
	clock = clock.Add(SASLExchangeTTL + time.Second)

	if _, ok := live.take("1"); ok {
		t.Error("an abandoned exchange survived its deadline")
	}
	if n := len(live.m); n != 0 {
		t.Errorf("%d exchanges left in the map, want none", n)
	}
}

// A conversation that is still fresh is still there: the deadline must not eat
// the ones being stepped through.
func TestAFreshExchangeIsKept(t *testing.T) {
	live := newExchanges()
	clock := time.Now()
	live.now = func() time.Time { return clock }

	live.put("1", &saslExchange{mech: MechScramSha256})
	clock = clock.Add(SASLExchangeTTL / 2)
	if _, ok := live.take("1"); !ok {
		t.Error("a conversation inside its deadline was dropped")
	}
}

// Stepping through a conversation keeps it alive: the deadline is aimed at a
// client that walked away, not at one that is slow.
func TestSteppingExtendsTheDeadline(t *testing.T) {
	live := newExchanges()
	clock := time.Now()
	live.now = func() time.Time { return clock }

	live.put("1", &saslExchange{mech: MechScramSha256})
	for i := 0; i < 5; i++ {
		clock = clock.Add(SASLExchangeTTL - time.Second)
		if _, ok := live.take("1"); !ok {
			t.Fatalf("a conversation still being stepped was dropped at round %d", i)
		}
	}
}

// A late CONT for a dropped conversation is told so by name, rather than being
// answered as if the exchange were still open.
func TestALateContinuationSaysThereIsNoExchange(t *testing.T) {
	srv := NewServer(scramChain(t, "alice", "hunter2"))
	r := newRelaySession(t, srv)
	line := r.cont([]byte("c=biws,r=nonce,p=proof"))
	if !strings.HasPrefix(line, "FAIL\t") {
		t.Fatalf("answered %q, want FAIL", line)
	}
	if !strings.Contains(line, "no-such-exchange") {
		t.Errorf("reply %q does not say the exchange is gone", line)
	}
}

// CANCEL frees the id at once, so an aborted AUTHENTICATE does not wait out
// the deadline.
func TestCancelFreesTheExchange(t *testing.T) {
	srv := NewServer(scramChain(t, "alice", "hunter2"))
	d := newScramClientDriver(MechScramSha256, "alice", "hunter2", nil)
	r := newRelaySession(t, srv)
	if line := r.begin(MechScramSha256, d.clientFirst(), nil); !strings.HasPrefix(line, "CONT\t") {
		t.Fatalf("the exchange did not start: %q", line)
	}
	fmt.Fprintf(r.conn, "CANCEL\t%s\n", r.id)

	line := r.cont([]byte("c=biws,r=nonce,p=proof"))
	if !strings.Contains(line, "no-such-exchange") {
		t.Errorf("after CANCEL the exchange answered %q, want no-such-exchange", line)
	}
}

// tokenRecorder is a token store that hands out a known value, so a row can
// see whether the reply carried one.
type tokenRecorder struct{ issued int }

func (t *tokenRecorder) Issue(user, session, service string) (string, error) {
	t.issued++
	return "tok-" + user, nil
}

func (t *tokenRecorder) Validate(tok string) (string, string, string, bool) {
	return strings.TrimPrefix(tok, "tok-"), "s1", "imap", true
}

// A SCRAM login must carry the token the password path carries: the backend
// verifies it, so a session without one cannot open (#1733).
func TestAScramLoginCarriesAToken(t *testing.T) {
	store := &tokenRecorder{}
	srv := NewServer(scramChain(t, "alice", "hunter2"), WithTokenStore(store))
	line := runSCRAM(t, srv, MechScramSha256, "alice", "hunter2", nil)
	if !strings.HasPrefix(line, "OK\t") {
		t.Fatalf("exchange ended %q, want OK", line)
	}
	if !strings.Contains(line, "token=tok-alice") {
		t.Errorf("reply %q carries no token; the backend would refuse the session", line)
	}
	if store.issued != 1 {
		t.Errorf("the store issued %d tokens, want one", store.issued)
	}
}
