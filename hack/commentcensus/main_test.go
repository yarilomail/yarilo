package main

import (
	"reflect"
	"testing"
)

func TestDashLines(t *testing.T) {
	allowed := map[dashAllowance]bool{
		{File: "a_test.go", Text: `{"split", "foo` + emDash + `bar"},`}: true,
		{File: "data/"}:     true,
		{File: "words.txt"}: true,
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
			name: "a line allowance does not cover the rest of its file",
			file: "a_test.go",
			src:  "\t{\"split\", \"foo" + emDash + "bar\"},\n// one " + emDash + " two\n",
			want: []string{"a_test.go:2: // one " + emDash + " two"},
		},
		{
			name: "the allowed text in another file is reported",
			file: "b_test.go",
			src:  "\t{\"split\", \"foo" + emDash + "bar\"},\n",
			want: []string{"b_test.go:1: {\"split\", \"foo" + emDash + "bar\"},"},
		},
		{
			name: "a file under an allowed directory passes whole",
			file: "data/stopwords_uk.txt",
			src:  "one " + emDash + " two\nthree" + emDash + "\n",
		},
		{
			name: "a directory sharing the prefix is not allowed",
			file: "data2/stopwords_uk.txt",
			src:  "one " + emDash + " two\n",
			want: []string{"data2/stopwords_uk.txt:1: one " + emDash + " two"},
		},
		{
			name: "an allowed file passes whole",
			file: "words.txt",
			src:  "one " + emDash + " two\n",
		},
		{
			name: "an allowed file name does not cover a directory of that name",
			file: "words.txt/x.md",
			src:  "one " + emDash + " two\n",
			want: []string{"words.txt/x.md:1: one " + emDash + " two"},
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
