package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

func export(t *testing.T) map[string]setting {
	t.Helper()
	var out []setting
	collect("", reflect.ValueOf(config.Defaults()).Elem(), &out)
	byKey := map[string]setting{}
	for _, s := range out {
		if _, dup := byKey[s.Key]; dup {
			t.Fatalf("%s exported twice", s.Key)
		}
		byKey[s.Key] = s
	}
	return byKey
}

func TestExport(t *testing.T) {
	got := export(t)
	for _, want := range []setting{
		{Key: "general.ssl.ssl_min_protocol", Section: "general.ssl", Type: "string", Kind: "string", Default: "TLS1.2"},
		{Key: "login.login_proxy_timeout", Section: "login", Type: "int", Kind: "integer", Default: 30},
		{Key: "general.ssl.ssl_prefer_server_ciphers", Section: "general.ssl", Type: "bool", Kind: "boolean", Default: false},
		{Key: "general.haproxy.haproxy_trusted_networks", Section: "general.haproxy", Type: "[]string", Kind: "list",
			Default: []string{"127.0.0.1/32", "10.0.0.0/8"}},
	} {
		if s, ok := got[want.Key]; !ok || !reflect.DeepEqual(s, want) {
			t.Errorf("%s: got %+v, want %+v", want.Key, s, want)
		}
	}
}

func TestExportShapes(t *testing.T) {
	got := export(t)
	// A pointer section with no default still lists its keys, a struct list uses [].
	for _, key := range []string{"services.imap.ssl.ssl_min_protocol", "acl.global[].mailbox"} {
		if _, ok := got[key]; !ok {
			t.Errorf("%s missing", key)
		}
	}
	// The raw string is the key; its parsed twin is tagged koanf:"-" and must not appear.
	n := 0
	for key, s := range got {
		if strings.HasSuffix(key, ".jmap_max_size_upload") {
			n++
			if s.Kind != "string" {
				t.Errorf("%s: kind %q, want string", key, s.Kind)
			}
		}
	}
	if n != 1 {
		t.Errorf("jmap_max_size_upload exported %d times, want once", n)
	}
	if s := got["auth.oauth2[].oauth2_mode"]; s.Kind != "string" {
		t.Errorf("named type kind: got %q, want string", s.Kind)
	}
}
