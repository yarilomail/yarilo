package imap_test

import (
	"fmt"
	"strings"
	"testing"
)

// A mailbox whose subscribed descendant the pattern does not reach is answered
// by LSUB with \Noselect (RFC 3501 §6.3.9) and by LIST (SUBSCRIBED
// RECURSIVEMATCH) with CHILDINFO (RFC 5258 §3.5); a plain LIST (SUBSCRIBED)
// leaves it out, and a pattern that reaches the descendant lists that instead.
func TestLsubAndRecursiveMatchNameTheParent(t *testing.T) {
	// p alone: only p/c was created, so p is no mailbox; p too: it is one.
	absent := []string{"CREATE p/c", "SUBSCRIBE p/c", "UNSUBSCRIBE p"}
	present := []string{"CREATE p", "CREATE p/c", "SUBSCRIBE p/c", "UNSUBSCRIBE p"}
	for _, tc := range []struct {
		setup []string
		cmd   string
		want  []string
	}{
		{absent, `LSUB "" %`, []string{`* LSUB (\Noselect) "/" "p"`}},
		{absent, `LIST (SUBSCRIBED RECURSIVEMATCH) "" %`, []string{`* LIST (\NonExistent) "/" "p" (CHILDINFO ("SUBSCRIBED"))`}},
		{absent, `LIST (SUBSCRIBED) "" %`, nil},
		{absent, `LSUB "" *`, []string{`* LSUB () "/" "p/c"`}},
		{absent, `LSUB "" p/%`, []string{`* LSUB () "/" "p/c"`}},
		{present, `LSUB "" %`, []string{`\Noselect`, `"/" "p"`}},
		{present, `LIST (SUBSCRIBED RECURSIVEMATCH) "" %`, []string{`"/" "p" (CHILDINFO ("SUBSCRIBED"))`}},
		{present, `LIST (SUBSCRIBED) "" %`, nil},
		{present, `LSUB "" *`, []string{`* LSUB () "/" "p/c"`}},
	} {
		t.Run(fmt.Sprintf("%d %s", len(tc.setup), tc.cmd), func(t *testing.T) {
			a, ard, _ := recentServer(t)
			for i, c := range tc.setup {
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
				if strings.Contains(l, "INBOX") {
					continue
				}
				rows = append(rows, l)
			}
			if (len(tc.want) > 0) != (len(rows) == 1) || (len(tc.want) == 0 && len(rows) != 0) {
				t.Fatalf("%s answered %q, want rows containing %q", tc.cmd, rows, tc.want)
			}
			if len(tc.want) > 0 {
				for _, w := range tc.want {
					if !strings.Contains(rows[0], w) {
						t.Errorf("%s row %q, want it to contain %q", tc.cmd, rows[0], w)
					}
				}
			}
		})
	}
}
