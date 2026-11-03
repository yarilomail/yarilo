package redisopt

import (
	"slices"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name, url, password, want string
		bad                       bool
	}{
		{name: "no password anywhere", url: "redis://r:6379/0"},
		{name: "separate password", url: "redis://r:6379/0", password: "s3cr:t@/x", want: "s3cr:t@/x"},
		{name: "separate password wins over the URL's", url: "redis://:old@r:6379/0", password: "new", want: "new"},
		{name: "URL password kept when none is given", url: "redis://:old@r:6379/0", want: "old"},
		{name: "bad URL", url: "http://r", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt, err := Parse(tc.url, tc.password)
			if tc.bad {
				if err == nil {
					t.Fatal("parsed")
				}
				return
			}
			if err != nil || opt.Password != tc.want {
				t.Fatalf("password %q, err %v; want %q", opt.Password, err, tc.want)
			}
		})
	}
}

// With Sentinels named the client finds the master through them, carrying the
// URL's password and db; without them it is today's client on the URL's
// address, option for option (#2505).
func TestClientMode(t *testing.T) {
	sentinel := Sentinel{Addrs: []string{"s1:26379", "s2:26379"}, Master: "mymaster", User: "sentinel", Password: "spw"}
	for _, tc := range []struct {
		name     string
		sentinel Sentinel
		wantAddr string
	}{
		{name: "one address", wantAddr: "r:6379"},
		{name: "through Sentinel", sentinel: sentinel, wantAddr: "FailoverClient"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt, err := Parse("redis://r:6379/3", "dpw")
			if err != nil {
				t.Fatal(err)
			}
			c := Client(opt, tc.sentinel)
			t.Cleanup(func() { _ = c.Close() })
			rc, ok := c.(*redis.Client)
			if !ok {
				t.Fatalf("client %T", c)
			}
			got := rc.Options()
			if got.Addr != tc.wantAddr || got.Password != "dpw" || got.DB != 3 {
				t.Errorf("addr %q password %q db %d; want %q, the URL's password and db 3", got.Addr, got.Password, got.DB, tc.wantAddr)
			}
		})
	}
}

func TestFailoverOptions(t *testing.T) {
	opt, err := Parse("redis://u@r:6379/2", "dpw")
	if err != nil {
		t.Fatal(err)
	}
	s := Sentinel{Addrs: []string{"s1:26379"}, Master: "mymaster", User: "sentinel", Password: "spw"}
	got := FailoverOptions(opt, s)
	want := redis.FailoverOptions{MasterName: "mymaster", SentinelAddrs: []string{"s1:26379"}, SentinelUsername: "sentinel", SentinelPassword: "spw", Username: "u", Password: "dpw", DB: 2}
	if got.MasterName != want.MasterName || !slices.Equal(got.SentinelAddrs, want.SentinelAddrs) || got.SentinelUsername != want.SentinelUsername ||
		got.SentinelPassword != want.SentinelPassword || got.Username != want.Username || got.Password != want.Password || got.DB != want.DB {
		t.Errorf("failover options = %+v, want %+v", *got, want)
	}
}

// Half a Sentinel configuration is refused, naming the key that is missing.
func TestSentinelCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		s       Sentinel
		wantErr string
	}{
		{name: "none"},
		{name: "both", s: Sentinel{Addrs: []string{"s:26379"}, Master: "m"}},
		{name: "addresses alone", s: Sentinel{Addrs: []string{"s:26379"}}, wantErr: "locks_service.sentinel_master"},
		{name: "master alone", s: Sentinel{Master: "m"}, wantErr: "locks_service.sentinel_addrs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Check("locks_service.")
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.wantErr)) {
				t.Errorf("Check = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestSentinelFromSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings map[string]any
		want     []string
	}{
		{name: "a YAML list", settings: map[string]any{"sentinel_addrs": []any{"a:1", "b:2"}}, want: []string{"a:1", "b:2"}},
		{name: "a comma-separated string", settings: map[string]any{"sentinel_addrs": "a:1, b:2"}, want: []string{"a:1", "b:2"}},
		{name: "none", settings: map[string]any{"addr": "r:6379"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.settings["sentinel_master"], tc.settings["sentinel_user"], tc.settings["sentinel_password"] = "m", "u", "p"
			got := SentinelFromSettings(tc.settings)
			if !slices.Equal(got.Addrs, tc.want) || got.Master != "m" || got.User != "u" || got.Password != "p" {
				t.Errorf("got %+v, want addrs %v and m/u/p", got, tc.want)
			}
		})
	}
}
