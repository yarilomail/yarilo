package file

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A new folder takes its GUID from WithGUIDSource; without one it is random (#2279).
func TestWithGUIDSourceNamesNewFolders(t *testing.T) {
	fixed := [16]byte{0xbe, 0x7c, 15: 1}
	cases := []struct {
		name string
		opts []Option
		want func([16]byte) bool
	}{
		{"with a source", []Option{WithGUIDSource(func() [16]byte { return fixed })}, func(g [16]byte) bool { return g == fixed }},
		{"without one", nil, func(g [16]byte) bool { return g != fixed && g != [16]byte{} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			ui := New(c.opts...).OpenUser(&mailbox.UserInfo{Username: testUser, Home: testHome(root, testUser)})
			defer ui.Close() //nolint:errcheck
			f, err := ui.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}
			if !c.want(f.GUID) {
				t.Errorf("folder GUID %x", f.GUID)
			}
		})
	}
}
