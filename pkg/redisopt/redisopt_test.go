package redisopt

import "testing"

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
