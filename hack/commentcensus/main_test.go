package main

import (
	"reflect"
	"testing"
)

func TestDashLines(t *testing.T) {
	allowed := map[dashAllowance]bool{
		{File: "a_test.go", Text: `{"split", "foo` + emDash + `bar"},`}: true,
	}
	cases := []struct {
		name string
		file string
		src  string
		want []string
	}{
		{
			name: "a dash in a comment is reported with its line",
			file: "x.go",
			src:  "package x\n\n// one " + emDash + " two\n",
			want: []string{"x.go:3: // one " + emDash + " two"},
		},
		{
			name: "an allowed line passes in its own file",
			file: "a_test.go",
			src:  "\t{\"split\", \"foo" + emDash + "bar\"},\n",
		},
		{
			name: "the allowed text in another file is reported",
			file: "b_test.go",
			src:  "\t{\"split\", \"foo" + emDash + "bar\"},\n",
			want: []string{"b_test.go:1: {\"split\", \"foo" + emDash + "bar\"},"},
		},
		{
			name: "the escape written out is not the character",
			file: "x.go",
			src:  "const d = \"\\u2014\"\n",
		},
		{
			name: "a hyphen is not a dash",
			file: "x.md",
			src:  "one - two\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dashLines(tc.file, []byte(tc.src), allowed); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("dashLines = %q, want %q", got, tc.want)
			}
		})
	}
}
