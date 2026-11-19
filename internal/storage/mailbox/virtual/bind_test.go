package virtual

import (
	"strings"
	"testing"
)

func lines(patterns ...string) *Config {
	cfg := &Config{}
	for _, p := range patterns {
		b := Box{Pattern: strings.TrimPrefix(p, "-"), Negative: strings.HasPrefix(p, "-")}
		cfg.Boxes = append(cfg.Boxes, b)
	}
	return cfg
}

func binder(declared ...Declared) *resolver {
	return &resolver{u: &userMailbox{opts: Options{Declared: declared}}}
}

var public = Declared{Prefix: "Public/", HasStore: true}

// A pattern belongs to the namespace its name resolves to: the longest declared
// prefix, INBOX and everything else the personal one (#2603 rule 1).
func TestAPatternBindsToItsNamespace(t *testing.T) {
	r := binder(public, Declared{Prefix: "Public/Team/", HasStore: true}, Declared{Prefix: "user/%u/", OwnerTemplated: true, HasStore: true})
	got, err := r.bind(lines("*", "INBOX", "Public/*", "Public/Team/*", "user/*/Shared", "user/alice/INBOX", "Publicity"))
	if err != nil {
		t.Fatal(err)
	}
	for pattern, want := range map[string]string{
		"*": "", "INBOX": "", "Publicity": "",
		"Public/*": "Public/", "Public/Team/*": "Public/Team/",
		"user/*/Shared": "user/%u/", "user/alice/INBOX": "user/%u/",
	} {
		if got[pattern] != want {
			t.Errorf("%q binds to %q, want %q", pattern, got[pattern], want)
		}
	}
}

// "*" takes the personal folders only; Public/ is reached by a line naming it.
func TestAWildcardReachesOnlyItsNamespace(t *testing.T) {
	cfg := lines("*", "Public/News")
	bound, err := binder(public).bind(cfg)
	if err != nil {
		t.Fatal(err)
	}
	personal := func(p string) bool { return bound[p] == "" }
	pub := func(p string) bool { return bound[p] == "Public/" }
	for _, tc := range []struct {
		folder       string
		in           func(string) bool
		want, wantEx bool
	}{
		{"Public/Other", pub, false, false},
		{"Public/News", pub, true, true},
		{"Archive", personal, true, false},
	} {
		got, ex := SelectsIn(cfg, tc.folder, tc.in)
		if got != tc.want || ex != tc.wantEx {
			t.Errorf("%s: selected %v explicit %v, want %v %v", tc.folder, got, ex, tc.want, tc.wantEx)
		}
	}
}

// A pattern under a namespace with no store is a configuration error naming
// the pattern, not a silent skip (#2603 rule 3).
func TestAPatternUnderANamespaceWithoutAStoreIsAnError(t *testing.T) {
	_, err := binder(public, Declared{Prefix: "Other/"}).bind(lines("Other/News"))
	if err == nil || !strings.Contains(err.Error(), `"Other/News"`) {
		t.Errorf("bind = %v, want an error naming Other/News", err)
	}
}

// A negative line takes out only what its own namespace holds.
func TestANegativeLineStaysInItsNamespace(t *testing.T) {
	cfg := lines("Public/*", "-*")
	bound, _ := binder(public).bind(cfg)
	if got, _ := SelectsIn(cfg, "Public/News", func(p string) bool { return bound[p] == "Public/" }); !got {
		t.Error("a personal negative line took out a public folder")
	}
}
