package mailbox

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAUsernameCannotStepOutOfTheRoot(t *testing.T) {
	r := &Resolver{Root: "/srv/mail", HomeTemplate: "%d/%n"}
	for _, tc := range []struct {
		user, want string
		bad        bool
	}{
		{user: "alice@example.com", want: filepath.Join("/srv/mail", "example.com", "alice")},
		{user: "alice", want: filepath.Join("/srv/mail", "alice")},
		{user: "", want: "/srv/mail"},
		{user: "a@../../escape", bad: true},
		{user: "../../escape@example.com", bad: true},
		{user: "..@example.com", bad: true},
		{user: "a@..", bad: true},
		{user: "a@.", bad: true},
		{user: "..", bad: true},
		{user: "@example.com", bad: true},
		{user: `a\b@example.com`, bad: true},
		{user: "a\x00b@example.com", bad: true},
	} {
		got, err := r.Resolve(tc.user, "")
		if tc.bad {
			if !errors.Is(err, ErrBadUsername) {
				t.Errorf("Resolve(%q) = %q, %v; want ErrBadUsername", tc.user, got, err)
			}
			if _, err := r.UserInfo(tc.user, ""); !errors.Is(err, ErrBadUsername) {
				t.Errorf("UserInfo(%q) err = %v, want ErrBadUsername", tc.user, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", tc.user, got, err, tc.want)
		}
	}
}
