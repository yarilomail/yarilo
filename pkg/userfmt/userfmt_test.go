package userfmt

import "testing"

func TestFormat(t *testing.T) {
	tests := []struct {
		name     string
		template string
		user     string
		want     string
		wantErr  bool
	}{
		{"identity default", "", "Alice@Example.com", "Alice@Example.com", false},
		{"identity %u", "%u", "Alice@Example.com", "Alice@Example.com", false},
		{"identity %{user}", "%{user}", "Alice@Example.com", "Alice@Example.com", false},
		{"lowercase %Lu", "%Lu", "Alice@Example.com", "alice@example.com", false},
		{"localpart %n", "%n", "alice@example.com", "alice", false},
		{"localpart %Ln", "%Ln", "ALICE@example.com", "alice", false},
		{"localpart no @", "%n", "alice", "alice", false},
		{"domain %d", "%d", "alice@example.com", "example.com", false},
		{"domain %Ld", "%Ld", "alice@Example.COM", "example.com", false},
		{"domain no @", "%d", "alice", "", false},
		{"mixed %n@%d", "%n@%d", "alice@example.com", "alice@example.com", false},
		{"literal text", "prefix-%u-suffix", "alice", "prefix-alice-suffix", false},
		{"trailing %", "alice%", "x", "", true},
		{"unknown %X", "%X", "x", "", true},
		{"unterminated %{", "%{user", "x", "", true},
		{"unknown %{foo}", "%{foo}", "x", "", true},
		{"trailing %L", "%L", "x", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Format(tc.template, tc.user)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
