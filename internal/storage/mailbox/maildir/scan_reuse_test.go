package maildir

import (
	"strings"
	"testing"
)

// A second walk reads no file it has already parsed: the parsed record stands
// until the name does not (#1800).
func TestASecondWalkReadsOnlyWhatIsNew(t *testing.T) {
	box, _, _ := recSetup(t)
	const body = "From: a@b\r\nSubject: x\r\n\r\nbody\r\n"
	for i := 1; i <= 20; i++ {
		name, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		if _, aerr := box.AssignUID("INBOX", name, uint32(i)); aerr != nil {
			t.Fatal(aerr)
		}
	}

	ResetScanStats()
	first, err := box.Scan("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 20 {
		t.Fatalf("the first walk found %d messages, want 20", len(first))
	}
	if got := ScanStats(); got != 20 {
		t.Fatalf("the first walk read %d files, want 20", got)
	}

	ResetScanStats()
	second, err := box.Scan("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 20 {
		t.Fatalf("the second walk found %d messages, want 20", len(second))
	}
	if got := ScanStats(); got != 0 {
		t.Errorf("the second walk read %d files, want none", got)
	}

	// And a message that arrives after it is read once, not the whole folder.
	fresh, _, _, serr := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if serr != nil {
		t.Fatal(serr)
	}
	if _, aerr := box.AssignUID("INBOX", fresh, 21); aerr != nil {
		t.Fatal(aerr)
	}
	ResetScanStats()
	if _, err := box.Scan("INBOX"); err != nil {
		t.Fatal(err)
	}
	if got := ScanStats(); got != 1 {
		t.Errorf("the walk after one arrival read %d files, want 1", got)
	}
}

// A flag write renames the file, so the record for the old name is not reused
// for the new one.
func TestARenamedMessageIsReadAgain(t *testing.T) {
	box, _, _ := recSetup(t)
	const body = "From: a@b\r\nSubject: x\r\n\r\nbody\r\n"
	name, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := box.AssignUID("INBOX", name, 1); aerr != nil {
		t.Fatal(aerr)
	}
	if _, err := box.Scan("INBOX"); err != nil {
		t.Fatal(err)
	}
	if _, werr := box.WriteFlags("INBOX", name, []string{`\Seen`}, nil); werr != nil {
		t.Fatal(werr)
	}

	ResetScanStats()
	recs, err := box.Scan("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if got := ScanStats(); got != 1 {
		t.Errorf("the walk after a rename read %d files, want 1", got)
	}
	if len(recs) != 1 {
		t.Fatalf("the folder holds %d records", len(recs))
	}
	if !hasFlagName(recs[0].Flags, `\Seen`) {
		t.Errorf("the record kept the flags of the old name: %v", recs[0].Flags)
	}
}

func hasFlagName(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// A GUID override written between two walks reaches the second: it renames
// nothing, so a record reused whole would keep the derived value (#1800).
func TestAnOverrideBetweenWalksReachesTheSecond(t *testing.T) {
	box, _, _ := recSetup(t)
	const body = "From: a@b\r\nSubject: x\r\n\r\nbody\r\n"
	name, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := box.AssignUID("INBOX", name, 1); aerr != nil {
		t.Fatal(aerr)
	}
	first, err := box.Scan("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	derived := first[0].GUID

	// The override an import or a backfill writes, with the file untouched.
	var pinned [16]byte
	copy(pinned[:], []byte("0123456789abcdef"))
	if werr := box.withMailboxLockSite("INBOX", "test", func() error {
		return box.appendUIDListLocked("INBOX", 1, name, true, pinned)
	}); werr != nil {
		t.Fatal(werr)
	}
	box.folderCacheFor("INBOX").invalidateUIDs("test")

	second, err := box.Scan("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if second[0].GUID == derived {
		t.Error("the second walk kept the derived GUID; the override never reached it")
	}
	if second[0].GUID != pinned {
		t.Errorf("the second walk reports %x, want the pinned %x", second[0].GUID, pinned)
	}
}
