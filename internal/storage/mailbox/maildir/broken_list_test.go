package maildir

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// listUIDs reads INBOX's list rows as uids, in file order.
func listUIDs(t *testing.T, box *userMailbox) []uint32 {
	t.Helper()
	var out []uint32
	for _, rec := range readUIDList(t, box) {
		n, err := strconv.ParseUint(strings.SplitN(rec, " ", 2)[0], 10, 32)
		if err != nil {
			t.Fatalf("row %q: %v", rec, err)
		}
		out = append(out, uint32(n))
	}
	return out
}

func ascending(uids []uint32) bool {
	for i := 1; i < len(uids); i++ {
		if uids[i] <= uids[i-1] {
			return false
		}
	}
	return true
}

// A list whose uids repeat or go backwards is set aside, as the reference drops
// it; every file comes back once, the index's records under their uids (#2086).
func TestABrokenListIsSetAsideAndTheFolderRebuilt(t *testing.T) {
	files := []string{"1700000001.M1P1.host", "1700000002.M2P2.host", "1700000003.M3P3.host", "1700000004.M4P4.host"}
	cases := []struct {
		name string
		rows []string
		// foreign is a list of another implementation's beside ours.
		foreign bool
	}{
		{name: "a uid named by two files", rows: []string{"1 :" + files[0] + ":2,", "2 :" + files[1] + ":2,", "2 :" + files[2] + ":2,", "5 :" + files[3] + ":2,"}},
		{name: "a uid below the one before", rows: []string{"1 :" + files[0] + ":2,", "5 :" + files[3] + ":2,", "2 :" + files[1] + ":2,", "6 :" + files[2] + ":2,"}},
		{name: "a foreign list beside it is not taken up", foreign: true, rows: []string{"1 :" + files[0] + ":2,", "2 :" + files[1] + ":2,", "2 :" + files[2] + ":2,", "5 :" + files[3] + ":2,"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			box, idx, folder := recSetup(t)
			for _, f := range files {
				deliverToCur(t, box, f+":2,", "From: a@b\r\n\r\n"+f+"\r\n")
			}
			scanned, err := box.Scan("INBOX")
			if err != nil {
				t.Fatal(err)
			}
			guidOf := map[string][16]byte{}
			for _, r := range scanned {
				guidOf[maildirBase(r.Filename)] = r.GUID
			}
			for i, f := range files[:2] {
				if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uint32(i + 1), Size: 30, GUID: guidOf[f]}); err != nil {
					t.Fatal(err)
				}
			}
			writeList(t, box, fmt.Sprintf("3 V%d N7 G00000000000000000000000000000000", folder.UIDValidity), tc.rows...)
			if tc.foreign {
				theirs := filepath.Join(box.folderPath("INBOX"), LegacyUIDListFileName)
				if err := os.WriteFile(theirs, []byte("3 V31337 N900 G00000000000000000000000000000000\n800 :"+files[0]+":2,\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
				t.Fatalf("the reconcile did not finish: %v", err)
			}
			got := folderUIDs(t, idx, folder)
			if len(got) != len(files) || got[0] != 1 || got[1] != 2 {
				t.Errorf("the folder holds uids %v, want all %d files with 1 and 2 kept", got, len(files))
			}
			aside, _ := filepath.Glob(box.uidListPath("INBOX") + ".broken.*")
			if len(aside) != 1 {
				t.Errorf("found %d set-aside lists, want the one", len(aside))
			}
			if uids := listUIDs(t, box); !ascending(uids) || len(uids) != len(files) {
				t.Errorf("the rebuilt list holds %v, want %d ascending uids", uids, len(files))
			}
			if v, _, _ := box.UIDSpace("INBOX"); tc.foreign && v == 31337 {
				t.Error("the foreign list was taken up when ours was set aside")
			}
		})
	}
}

// Rows land in uid order whatever order the saves finish in: one below the last
// goes through the rewrite, which sorts (#2086).
func TestTheListStaysAscendingWhateverOrderSavesFinish(t *testing.T) {
	t.Run("one save below the last", func(t *testing.T) {
		box, _ := newBox(t, "u@x.com")
		if err := box.Init(); err != nil {
			t.Fatal(err)
		}
		saveAndRecord(t, box, "INBOX", "From: a@b\r\n\r\nx\r\n", 11, nil)
		saveAndRecord(t, box, "INBOX", "From: c@d\r\n\r\ny\r\n", 10, nil)
		if uids := listUIDs(t, box); !ascending(uids) {
			t.Errorf("the list holds %v, want ascending", uids)
		}
	})
	t.Run("saves racing", func(t *testing.T) {
		box, _ := newBox(t, "u@x.com")
		if err := box.Init(); err != nil {
			t.Fatal(err)
		}
		const n = 24
		names := make([]string, n)
		for i := range names {
			body := fmt.Sprintf("From: a@b\r\n\r\n%d\r\n", i)
			name, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			names[i] = name
		}
		var wg sync.WaitGroup
		for i := range names {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if _, err := box.AssignUID("INBOX", names[i], uint32(n-i)); err != nil {
					t.Errorf("assign uid %d: %v", n-i, err)
				}
			}(i)
		}
		wg.Wait()
		if uids := listUIDs(t, box); !ascending(uids) || len(uids) != n {
			t.Errorf("the list holds %v, want %d ascending uids", uids, n)
		}
	})
}
