package config

import "testing"

// A namespace says where its indexes go, as the reference's mail_index_path
// does: one definition directory can then be shared, read-only, by every user.
func TestANamespaceSaysWhereItsIndexesGo(t *testing.T) {
	for _, tc := range []struct {
		name string
		ns   NamespaceConfig
		want string
		bad  bool
	}{
		{"the split spelling folds into the location",
			NamespaceConfig{Prefix: "Virtual/", MailDriver: "virtual", MailPath: "/etc/yarilo/virtual", MailIndexPath: "%h/index/virtual"},
			"virtual:/etc/yarilo/virtual:INDEX=%h/index/virtual", false},
		{"without it the location is the pair alone",
			NamespaceConfig{Prefix: "Virtual/", MailDriver: "virtual", MailPath: "/etc/yarilo/virtual"},
			"virtual:/etc/yarilo/virtual", false},
		{"an index path alone names a store this namespace does not",
			NamespaceConfig{Prefix: "Virtual/", MailIndexPath: "%h/index/virtual"}, "", true},
		{"a location spelled out keeps its own options",
			NamespaceConfig{Prefix: "Virtual/", Location: "virtual:/etc/yarilo/virtual:INDEX=%h/i"},
			"virtual:/etc/yarilo/virtual:INDEX=%h/i", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nss := []NamespaceConfig{tc.ns}
			err := foldNamespaceLocations(nss)
			if tc.bad {
				if err == nil {
					t.Fatalf("accepted %+v", tc.ns)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if nss[0].Location != tc.want {
				t.Errorf("location = %q, want %q", nss[0].Location, tc.want)
			}
		})
	}
}

// A driver nothing implements is refused: the storage layer falls back to
// maildir, so a typo would be mail written in another format.
func TestAnUnknownMailDriverIsRefused(t *testing.T) {
	for _, tc := range []struct {
		driver string
		bad    bool
	}{
		{"", false}, {"maildir", false}, {"mdbox", false}, {"sdbox", false},
		{"dbox", false}, {"virtual", false}, {"MDBOX", false},
		{"maildirr", true}, {"mbox", true}, {"sqlite", true},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			err := validateMailDriver(tc.driver)
			if (err != nil) != tc.bad {
				t.Errorf("validateMailDriver(%q) = %v, want refused %v", tc.driver, err, tc.bad)
			}
		})
	}
}
