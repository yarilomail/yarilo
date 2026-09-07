package imap

import (
	"testing"
)

// A file must survive until the last record naming it is expunged, otherwise
// expunging one member of a damaged pair strips the other's body.
func TestBodyRefsFreesOnLastRelease(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		file  string
		frees []bool // result of release() per successive call
	}{
		{
			name:  "sole record frees immediately",
			names: []string{"a"},
			file:  "a",
			frees: []bool{true},
		},
		{
			name:  "shared file frees only on the second release",
			names: []string{"a", "a"},
			file:  "a",
			frees: []bool{false, true},
		},
		{
			name:  "three records naming one file",
			names: []string{"a", "a", "a"},
			file:  "a",
			frees: []bool{false, false, true},
		},
		{
			name:  "distinct files are independent",
			names: []string{"a", "b"},
			file:  "a",
			frees: []bool{true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs := newBodyRefs(tt.names)
			for i, want := range tt.frees {
				if got := refs.fate(tt.file) == bodyFree; got != want {
					t.Errorf("fate #%d frees = %v, want %v", i+1, got, want)
				}
			}
		})
	}
}

// An empty filename means the record names no body, so there is nothing to free
// and nothing referring to one -- its own case since #1693.
func TestBodyRefsIgnoresEmptyFilename(t *testing.T) {
	refs := newBodyRefs([]string{""})
	if got := refs.fate(""); got != bodyNameless {
		t.Errorf("fate(\"\") = %v, want bodyNameless", got)
	}
}

// A file no record names is freeable: nothing else can be pointing at it, and
// the caller only releases a file it is expunging anyway.
func TestBodyRefsUnknownFileIsFreeable(t *testing.T) {
	refs := newBodyRefs([]string{"a"})
	if refs.fate("b") != bodyFree {
		t.Error("unknown file reported as still referenced")
	}
}
