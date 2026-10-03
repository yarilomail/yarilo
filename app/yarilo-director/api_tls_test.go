package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/director"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mtls"
	"github.com/yarilomail/yarilo/pkg/mtls/mtlstest"
)

// With internal TLS the admin API is mTLS for the admin role; the token stays (#2132).
func TestTheDirectorAPIIsMTLSForAdmin(t *testing.T) {
	ca := mtlstest.NewCA(t)
	sc, sk := ca.Role(t, mtls.RoleDirector)
	srvCfg, err := mtls.ServerConfig(sc, sk, ca.CAFile, mtls.ListenerDirectorAPI)
	if err != nil {
		t.Fatal(err)
	}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	free.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := startAPI(ctx, director.New(), config.DirectorAPIConfig{Listen: addr, Token: "t"}, srvCfg, func(err error) { t.Errorf("serve: %v", err) }); err != nil {
		t.Fatal(err)
	}

	get := func(scheme string, role mtls.Role) (int, error) {
		tr := &http.Transport{}
		if role != "" {
			c, k := ca.Role(t, role)
			cli, err := mtls.ClientConfig(c, k, ca.CAFile, mtlstest.ServerName, -1, 0)
			if err != nil {
				t.Fatal(err)
			}
			tr.TLSClientConfig = cli
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+addr+"/api/director/status", nil)
		req.Header.Set("Authorization", "Bearer t")
		resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	for _, tc := range []struct {
		scheme string
		role   mtls.Role
		want   int
	}{
		{"https", mtls.RoleAdmin, http.StatusOK},
		{"https", mtls.RoleIMAPLogin, 0},
		{"http", "", 0},
	} {
		code, err := get(tc.scheme, tc.role)
		if tc.want == 0 && code == http.StatusOK {
			t.Errorf("%s as %q got 200; want refused", tc.scheme, tc.role)
		}
		if tc.want != 0 && code != tc.want {
			t.Errorf("%s as %q: %d, %v; want %d", tc.scheme, tc.role, code, err, tc.want)
		}
	}
}
