package mailboxbase_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
)

// The walk budget weighs a walk at WalkBytesPerFile per message: a change that
// makes a walk dearer per file must show here, not quietly let walks past the
// budget the process was given (#2661).
func TestAWalkCostsWhatTheBudgetWeighs(t *testing.T) {
	if raceDetector {
		t.Skip("the race detector allocates on its own; the gate's plain run measures this")
	}
	for _, files := range []int{2000, 10000} {
		t.Run(fmt.Sprint(files), func(t *testing.T) {
			box, inbox := gateSetup(t)
			body := []byte("From: a@b\r\nSubject: x\r\n\r\nbody\r\n")
			for i := 0; i < files; i++ {
				name := fmt.Sprintf("17%08d.M%dP1.yarilo-backend-0,S=%d,W=%d:2,S", i, i, len(body), len(body))
				if err := os.WriteFile(filepath.Join(inbox, "cur", name), body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			settle(t, inbox, time.Now().Add(-time.Hour))
			if _, err := box.Folder("INBOX", 0); err != nil {
				t.Fatal(err)
			}
			// The walk of an unchanged folder by a process that remembers none.
			t.Cleanup(mailboxbase.SetTestSyncTokens(8))
			t.Cleanup(mailboxbase.SetTestNoIndexStamp())
			runtime.GC()
			defer debug.SetGCPercent(debug.SetGCPercent(-1))
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			if _, err := box.Folder("INBOX", 0); err != nil {
				t.Fatal(err)
			}
			runtime.ReadMemStats(&after)
			perFile := float64(after.TotalAlloc-before.TotalAlloc) / float64(files)
			t.Logf("%d files: %.0f B per file", files, perFile)
			if perFile > mailboxbase.WalkBytesPerFile {
				t.Errorf("a walk of %d files allocates %.0f B per file, more than the %d the budget weighs it at", files, perFile, mailboxbase.WalkBytesPerFile)
			}
		})
	}
}
