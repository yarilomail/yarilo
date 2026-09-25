package jmap

import (
	"strings"
	"testing"
)

// A bare word decoder knows three charsets and refuses the rest, which for this
// mail is not an edge case: windows-1251 and koi8 are ordinary here (#2008).
func TestEmailGetDecodesTheCharsetsMailUses(t *testing.T) {
	const cp1251Message = "From: =?windows-1251?B?zuvl6vHg7eTw?= <olek@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: =?windows-1251?B?z/Du7+7n6Paz/w==?=\r\n" +
		"Message-Id: <cp@example.com>\r\n" +
		"Date: Mon, 03 Aug 2026 10:00:00 +0000\r\n" +
		"\r\n" +
		"body\r\n"

	for _, tc := range []struct{ name, props string }{
		{"parsed", `["id","subject","from","references"]`}, // references forces the parse
		{"from the cache", envelopeProps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, id, _ := storedServerWithMessageAt(t, cp1251Message, 0)
			// Warm through the envelope path, so the cached row is read back.
			emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],"properties":`+envelopeProps+`}`)

			email := firstEmail(t, emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],
				"properties":`+tc.props+`}`))
			if got, _ := email["subject"].(string); got != "Пропозиція" {
				t.Errorf("subject = %q, want the decoded windows-1251 text", got)
			}
			from, _ := email["from"].([]any)
			if len(from) != 1 {
				t.Fatalf("from = %v", email["from"])
			}
			addr, _ := from[0].(map[string]any)
			if name, _ := addr["name"].(string); name != "Олександр" {
				t.Errorf("display name = %q, want the decoded windows-1251 text", name)
			}
			if strings.Contains(strings.Join([]string{addr["name"].(string)}, ""), "=?") {
				t.Errorf("the name reached the client as an encoded word: %v", addr["name"])
			}
		})
	}
}
