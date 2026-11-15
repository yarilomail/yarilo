package main

import (
	"bufio"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// folderServer answers SELECT and UID SEARCH/FETCH from a fixed map: the probe
// is in the folders it names, as many times as given.
func folderServer(t *testing.T, probes map[string]int, refuse ...string) *imapClient {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	t.Cleanup(func() { clientSide.Close(); serverSide.Close() })
	go func() {
		r := bufio.NewReader(serverSide)
		selected := ""
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
			verb, args, _ := strings.Cut(rest, " ")
			switch strings.ToUpper(verb) {
			case "SELECT":
				selected = strings.Trim(args, `"`)
				if slices.Contains(refuse, selected) {
					fmt.Fprintf(serverSide, "%s NO no such mailbox\r\n", tag) //nolint:errcheck
					continue
				}
				fmt.Fprintf(serverSide, "* 0 EXISTS\r\n") //nolint:errcheck
			case "UID":
				if strings.HasPrefix(strings.ToUpper(args), "SEARCH") {
					uids := []string{}
					for i := 1; i <= probes[selected]; i++ {
						uids = append(uids, fmt.Sprint(i))
					}
					fmt.Fprintf(serverSide, "* SEARCH %s\r\n", strings.Join(uids, " ")) //nolint:errcheck
				} else {
					fmt.Fprintf(serverSide, "* 1 FETCH (UID 1 BODY[HEADER.FIELDS (RECEIVED)] {%d}\r\n", len("Received: from x ([203.0.113.7]) in "+selected+"\r\n\r\n")) //nolint:errcheck
					fmt.Fprintf(serverSide, "Received: from x ([203.0.113.7]) in %s\r\n\r\n)\r\n", selected)                                                             //nolint:errcheck
				}
			}
			fmt.Fprintf(serverSide, "%s OK done\r\n", tag) //nolint:errcheck
		}
	}()
	return &imapClient{conn: clientSide, r: bufio.NewReader(clientSide)}
}

// The PROXY probe is found where spam policy put it, and the row says where;
// missing everywhere or ambiguous is a failure.
func TestFindProbeLooksInInboxAndJunk(t *testing.T) {
	old := *flagTimeout
	*flagTimeout = 100 * time.Millisecond
	t.Cleanup(func() { *flagTimeout = old })

	for _, tc := range []struct {
		name    string
		probes  map[string]int
		refuse  []string
		folder  string
		wantErr string
	}{
		{"in INBOX", map[string]int{"INBOX": 1}, nil, "INBOX", ""},
		{"filed into Junk", map[string]int{"Junk": 1}, nil, "Junk", ""},
		{"no Junk folder", map[string]int{"INBOX": 1}, []string{"Junk"}, "INBOX", ""},
		{"INBOX refused", map[string]int{"Junk": 1}, []string{"INBOX"}, "", "select INBOX"},
		{"nowhere", map[string]int{}, nil, "", "never became visible"},
		{"twice in Junk", map[string]int{"Junk": 2}, nil, "", "not identifiable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received, folder, err := findProbe(folderServer(t, tc.probes, tc.refuse...), "xproxy-1")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("findProbe: %v", err)
			}
			if folder != tc.folder {
				t.Errorf("folder = %q, want %q", folder, tc.folder)
			}
			if !strings.Contains(received, "203.0.113.7") || !strings.Contains(received, "in "+tc.folder) {
				t.Errorf("received = %q, want the probe's Received from %s", received, tc.folder)
			}
		})
	}
}
