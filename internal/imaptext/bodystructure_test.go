package imaptext

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

// The bytes again: what a client is shown is what the cache holds, so the
// writer is checked against the reference's own output, not against itself
// (imap-bodystructure.c:26-283).
func TestWriteBodyStructureMatchesTheReferencesBytes(t *testing.T) {
	tests := []struct {
		name     string
		bs       imaplib.BodyStructure
		extended bool
		want     string
	}{
		{
			name: "a plain text part, BODY",
			bs: &imaplib.BodyStructureSinglePart{
				Type: "text", Subtype: "plain", Encoding: "7bit", Size: 42,
				Text: &imaplib.BodyStructureText{NumLines: 3},
			},
			want: `"text" "plain" ("charset" "us-ascii") NIL NIL "7bit" 42 3`,
		},
		{
			name: "a text part naming its charset",
			bs: &imaplib.BodyStructureSinglePart{
				Type: "text", Subtype: "plain", Params: map[string]string{"charset": "utf-8"},
				Encoding: "8bit", Size: 10, Text: &imaplib.BodyStructureText{NumLines: 1},
			},
			want: `"text" "plain" ("charset" "utf-8") NIL NIL "8bit" 10 1`,
		},
		{
			name: "a binary part has no line count and no default charset",
			bs: &imaplib.BodyStructureSinglePart{
				Type: "application", Subtype: "pdf", Encoding: "base64", Size: 1000,
			},
			want: `"application" "pdf" NIL NIL NIL "base64" 1000`,
		},
		{
			name:     "the same part as BODYSTRUCTURE",
			extended: true,
			bs: &imaplib.BodyStructureSinglePart{
				Type: "application", Subtype: "pdf", Encoding: "base64", Size: 1000,
				Extended: &imaplib.BodyStructureSinglePartExt{
					Disposition: &imaplib.BodyStructureDisposition{
						Value: "attachment", Params: map[string]string{"filename": "a.pdf"},
					},
				},
			},
			want: `"application" "pdf" NIL NIL NIL "base64" 1000 NIL ("attachment" ("filename" "a.pdf")) NIL NIL`,
		},
		{
			name: "two parts under a multipart",
			bs: &imaplib.BodyStructureMultiPart{
				Subtype: "mixed",
				Children: []imaplib.BodyStructure{
					&imaplib.BodyStructureSinglePart{Type: "text", Subtype: "plain", Encoding: "7bit", Size: 5,
						Text: &imaplib.BodyStructureText{NumLines: 1}},
					&imaplib.BodyStructureSinglePart{Type: "image", Subtype: "png", Encoding: "base64", Size: 99},
				},
			},
			want: `("text" "plain" ("charset" "us-ascii") NIL NIL "7bit" 5 1)("image" "png" NIL NIL NIL "base64" 99) "mixed"`,
		},
		{
			name:     "a multipart with no children is written as an empty text part",
			extended: true,
			bs:       &imaplib.BodyStructureMultiPart{Subtype: "mixed"},
			want:     `("text" "plain" ("charset" "us-ascii") NIL NIL "7bit" 0 0 NIL NIL NIL NIL) "mixed" NIL NIL NIL NIL`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := WriteBodyStructure(tc.bs, tc.extended)
			if !ok {
				t.Fatal("the structure was refused")
			}
			if got != tc.want {
				t.Errorf("wrote\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// A structure the reference wrote is read as it stands, nested parts included.
func TestParseBodyStructureWrittenByTheReference(t *testing.T) {
	const s = `("text" "plain" ("charset" "utf-8") NIL NIL "quoted-printable" 415 12 NIL NIL NIL NIL)("message" "rfc822" NIL NIL NIL "7bit" 900 ("Fri, 18 Sep 2026 09:12:00 +0300" "fwd" ((NIL NIL "a" "b.ua")) NIL NIL NIL NIL NIL NIL "<x@b.ua>") ("text" "plain" ("charset" "us-ascii") NIL NIL "7bit" 100 4) 20 NIL NIL NIL NIL) "mixed" NIL NIL NIL NIL`
	bs, ok := ParseBodyStructure(s)
	if !ok {
		t.Fatal("a reference-written body structure did not parse")
	}
	mp, ok := bs.(*imaplib.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("got %T, want a multipart", bs)
	}
	if mp.Subtype != "mixed" || len(mp.Children) != 2 {
		t.Fatalf("subtype %q with %d children", mp.Subtype, len(mp.Children))
	}
	first, ok := mp.Children[0].(*imaplib.BodyStructureSinglePart)
	if !ok || first.Size != 415 || first.Text == nil || first.Text.NumLines != 12 {
		t.Errorf("first child = %+v", mp.Children[0])
	}
	second, ok := mp.Children[1].(*imaplib.BodyStructureSinglePart)
	if !ok || second.MessageRFC822 == nil {
		t.Fatalf("second child = %+v", mp.Children[1])
	}
	if second.MessageRFC822.Envelope.MessageID != "x@b.ua" {
		t.Errorf("nested envelope = %+v", second.MessageRFC822.Envelope)
	}
	if second.MessageRFC822.NumLines != 20 {
		t.Errorf("nested line count = %d, want 20", second.MessageRFC822.NumLines)
	}
}

func TestBodyStructureRoundTrip(t *testing.T) {
	bs := &imaplib.BodyStructureMultiPart{
		Subtype: "alternative",
		Children: []imaplib.BodyStructure{
			&imaplib.BodyStructureSinglePart{Type: "text", Subtype: "plain",
				Params: map[string]string{"charset": "utf-8"}, Encoding: "8bit", Size: 12,
				Text: &imaplib.BodyStructureText{NumLines: 2}},
			&imaplib.BodyStructureSinglePart{Type: "text", Subtype: "html",
				Params: map[string]string{"charset": "utf-8"}, Encoding: "8bit", Size: 40,
				Text: &imaplib.BodyStructureText{NumLines: 5}},
		},
		Extended: &imaplib.BodyStructureMultiPartExt{},
	}
	s, ok := WriteBodyStructure(bs, true)
	if !ok {
		t.Fatal("the structure was refused")
	}
	back, ok := ParseBodyStructure(s)
	if !ok {
		t.Fatalf("what we wrote did not parse: %q", s)
	}
	if again, _ := WriteBodyStructure(back, true); again != s {
		t.Errorf("a second pass differs:\n  %q\n  %q", again, s)
	}
}

// A nested structure from elsewhere is data, and must not cost the process its
// stack.
func TestParseBodyStructureRefusesEndlessNesting(t *testing.T) {
	s := ""
	for i := 0; i < 200; i++ {
		s = "(" + s
	}
	if _, ok := ParseBodyStructure(s); ok {
		t.Error("an unterminated nest parsed")
	}
	deep := `"text" "plain" NIL NIL NIL "7bit" 0 0`
	for i := 0; i < 200; i++ {
		deep = "(" + deep + `) "mixed"`
	}
	if _, ok := ParseBodyStructure(deep); ok {
		t.Error("a structure nested past the bound parsed")
	}
}
