package main

import (
	"bufio"
	"encoding/base64"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-sasl"
)

// scramIMAP answers one AUTHENTICATE over pipe with a real SCRAM-SHA-256
// server; tamper rewrites the server's messages before they are sent.
func scramIMAP(t *testing.T, creds *sasl.ScramCredentials, tamper func(step int, msg []byte) []byte) *imapClient {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	go func() {
		r := bufio.NewReader(server)
		srv := sasl.NewScramSha256Server(func(string) (*sasl.ScramCredentials, error) { return creds, nil })
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		tag, resp := fields[0], fields[len(fields)-1]
		for step := 0; ; step++ {
			in, _ := base64.StdEncoding.DecodeString(resp)
			out, done, err := srv.Next(in)
			if err != nil {
				_, _ = server.Write([]byte(tag + " NO authentication failed\r\n"))
				return
			}
			if tamper != nil {
				out = tamper(step, out)
			}
			_, _ = server.Write([]byte("+ " + base64.StdEncoding.EncodeToString(out) + "\r\n"))
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			resp = strings.TrimSpace(line)
			if done {
				_, _ = server.Write([]byte(tag + " OK logged in\r\n"))
				return
			}
		}
	}()
	return &imapClient{conn: client, r: bufio.NewReader(client)}
}

// The smoketest's own SCRAM client logs in where the password is right, is
// refused where it is not, and refuses a server whose signature or nonce does
// not hold, which is what makes its passing row mean something.
func TestSmokeSCRAMClient(t *testing.T) {
	creds, err := sasl.GenerateScramSha256Credentials("pw", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := scramIMAP(t, creds, nil).scramLogin("SCRAM-SHA-256", "alice", "pw", nil); err != nil {
		t.Errorf("right password: %v", err)
	}
	if err := scramIMAP(t, creds, nil).scramLogin("SCRAM-SHA-256", "alice", "nope", nil); err == nil {
		t.Error("a wrong password logged in")
	}
	forged := func(step int, msg []byte) []byte {
		if step == 1 {
			return []byte("v=" + base64.StdEncoding.EncodeToString(make([]byte, 32)))
		}
		return msg
	}
	if err := scramIMAP(t, creds, forged).scramLogin("SCRAM-SHA-256", "alice", "pw", nil); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("a forged server signature = %v, want it refused", err)
	}
	foreignNonce := func(step int, msg []byte) []byte {
		if step == 0 {
			return []byte(strings.Replace(string(msg), "r=yariloSmokeNonce", "r=someoneElse", 1))
		}
		return msg
	}
	if err := scramIMAP(t, creds, foreignNonce).scramLogin("SCRAM-SHA-256", "alice", "pw", nil); err == nil || !strings.Contains(err.Error(), "does not extend") {
		t.Errorf("a nonce that does not extend the client's = %v, want it refused", err)
	}
}

func TestLastProof(t *testing.T) {
	for in, want := range map[string]string{"c=biws,r=x,p=abc": "abc", "no proof": ""} {
		if got := lastProof(in); got != want {
			t.Errorf("lastProof(%q) = %q, want %q", in, got, want)
		}
	}
}
