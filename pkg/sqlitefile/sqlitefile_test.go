package sqlitefile

import "testing"

func TestPath(t *testing.T) {
	for _, tc := range []struct{ dsn, want string }{
		{"/var/lib/yarilo/users.db", "/var/lib/yarilo/users.db"},
		{"users.db?_pragma=foreign_keys(1)", "users.db"},
		{"file:/var/lib/yarilo/users.db", "/var/lib/yarilo/users.db"},
		{"file:///var/lib/yarilo/users.db?mode=rwc", "/var/lib/yarilo/users.db"},
		{"file://localhost/var/lib/my%20db.db", "/var/lib/my db.db"},
		{"file:/var/lib/yarilo/users.db?mode=ro", ""},
		{"file:/var/lib/yarilo/users.db?mode=rw", ""},
		{"file:mem?mode=memory", ""},
		{"file:/x.db?vfs=memdb", ""},
		{":memory:", ""},
		{"file::memory:", ""},
		{"", ""},
		{"file://localhost", ""},
		{"file:/x%zz.db", ""},
	} {
		if got := Path(tc.dsn); got != tc.want {
			t.Errorf("Path(%q) = %q, want %q", tc.dsn, got, tc.want)
		}
	}
}

func TestCreateReturnsAFailure(t *testing.T) {
	if err := Create("/nonexistent-yarilo-dir/users.db"); err == nil {
		t.Fatal("created a file in a directory that does not exist")
	}
}
