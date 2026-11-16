package imap_test

import (
	"fmt"
	"strings"
	"testing"
)

// A mailbox whose subscribed descendant the pattern does not reach is named by
// LIST (SUBSCRIBED RECURSIVEMATCH) with CHILDINFO (RFC 5258 §3.5), subscribed
// or not, existing or not; LSUB answers an unsubscribed one with \Noselect
// (RFC 3501 §6.3.9) and a subscribed one as itself. A plain LIST (SUBSCRIBED)
// and a pattern that reaches the descendant add nothing.
func TestLsubAndRecursiveMatchNameTheParent(t *testing.T) {
	setups := map[string][]string{
		"p absent":                {"CREATE p/c", "SUBSCRIBE p/c", "UNSUBSCRIBE p"},
		"p a mailbox":             {"CREATE p", "CREATE p/c", "SUBSCRIBE p/c", "UNSUBSCRIBE p"},
		"p subscribed, a mailbox": {"CREATE p", "CREATE p/c", "SUBSCRIBE p", "SUBSCRIBE p/c"},
		"p subscribed, absent":    {"CREATE p/c", "SUBSCRIBE p", "SUBSCRIBE p/c"},
	}
	childinfo := `(CHILDINFO ("SUBSCRIBED"))`
	for _, tc := range []struct {
		setup, cmd string
		// rows: one entry per row the answer must hold, each the substrings
		// that row carries; absent: substrings no row may carry.
		rows   [][]string
		absent []string
	}{
		{"p absent", `LSUB "" %`, [][]string{{`* LSUB (\Noselect) "/" "p"`}}, nil},
		{"p absent", `LIST (SUBSCRIBED RECURSIVEMATCH) "" %`, [][]string{{`\NonExistent`, `"/" "p" ` + childinfo}}, nil},
		{"p absent", `LIST (SUBSCRIBED) "" %`, nil, nil},
		{"p absent", `LSUB "" *`, [][]string{{`* LSUB () "/" "p/c"`}}, nil},
		{"p absent", `LSUB "" p/%`, [][]string{{`* LSUB () "/" "p/c"`}}, nil},
		{"p a mailbox", `LSUB "" %`, [][]string{{`\Noselect`, `"/" "p"`}}, nil},
		{"p a mailbox", `LIST (SUBSCRIBED RECURSIVEMATCH) "" %`, [][]string{{`"/" "p" ` + childinfo}}, []string{`\Subscribed`}},
		{"p a mailbox", `LIST (SUBSCRIBED) "" %`, nil, nil},
		{"p a mailbox", `LSUB "" *`, [][]string{{`* LSUB () "/" "p/c"`}}, nil},
		{"p subscribed, a mailbox", `LIST (SUBSCRIBED RECURSIVEMATCH) "" %`, [][]string{{`\Subscribed`, `"/" "p" ` + childinfo}}, nil},
		{"p subscribed, a mailbox", `LIST (SUBSCRIBED RECURSIVEMATCH) "" *`, [][]string{{`"/" "p"`}, {`"/" "p/c"`}}, []string{"CHILDINFO"}},
		{"p subscribed, a mailbox", `LSUB "" %`, [][]string{{`* LSUB () "/" "p"`}}, []string{`\Noselect`}},
		{"p subscribed, absent", `LIST (SUBSCRIBED RECURSIVEMATCH) "" %`, [][]string{{`\NonExistent`, `\Subscribed`, `"/" "p" ` + childinfo}}, nil},
		{"p subscribed, absent", `LSUB "" %`, [][]string{{`* LSUB () "/" "p"`}}, []string{`\Noselect`}},
	} {
		t.Run(tc.setup+" "+tc.cmd, func(t *testing.T) {
			a, ard, _ := recentServer(t)
			for i, c := range setups[tc.setup] {
				if _, got := send(t, a, ard, fmt.Sprintf("s%d", i), c); got != "OK" {
					t.Fatalf("setup %s: %s", c, got)
				}
			}
			lines, got := send(t, a, ard, "l1", tc.cmd)
			if got != "OK" {
				t.Fatalf("%s: %s", tc.cmd, got)
			}
			var rows []string
			for _, l := range lines {
				if !strings.Contains(l, "INBOX") {
					rows = append(rows, l)
				}
			}
			if len(rows) != len(tc.rows) {
				t.Fatalf("%s answered %q, want %d rows", tc.cmd, rows, len(tc.rows))
			}
			for i, want := range tc.rows {
				for _, w := range want {
					if !strings.Contains(rows[i], w) {
						t.Errorf("%s row %q, want it to carry %q", tc.cmd, rows[i], w)
					}
				}
			}
			for _, r := range rows {
				for _, w := range tc.absent {
					if strings.Contains(r, w) {
						t.Errorf("%s row %q carries %q", tc.cmd, r, w)
					}
				}
			}
		})
	}
}
