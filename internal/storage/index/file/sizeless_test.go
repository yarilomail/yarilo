package file

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// sizelessAfterTail is what a reader's search for records without a size
// allocates, and answers, on a folder of n whose vsize aggregate is current,
// past three appends another session journalled; sizeless says which of them
// carries no size.
func sizelessAfterTail(t *testing.T, n int, sizeless int) (uint64, []uint32, []uint32) {
	t.Helper()
	home := movedFolder(t, n)
	r, rf := acceptOpen(t, home)
	defer r.Close() //nolint:errcheck
	w, wf := acceptOpen(t, home)
	defer w.Close() //nolint:errcheck
	var want []uint32
	for i := range 3 {
		uid, err := w.AllocateUID(wf.ID)
		if err != nil {
			t.Fatal(err)
		}
		m := &mailbox.MessageMeta{UID: uid, GUID: [16]byte{byte(i + 1)}, Size: 1200, VSize: 1230, InternalDate: time.Unix(1700000000, 0)}
		if i == sizeless {
			m.Size, m.VSize = 0, 0 // as a foreign writer leaves it
			want = append(want, uid)
		}
		if err := w.AppendMessage(wf.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	baseImages.Delete(indexPathFor(home))
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	got, err := r.(mailbox.SizelessLister).SizelessUIDs(rf.ID)
	runtime.ReadMemStats(&m1)
	if err != nil {
		t.Fatal(err)
	}
	return m1.TotalAlloc - m0.TotalAlloc, got, want
}

// The delivery's quota check asks for records without a size on every
// delivery: it reads the records above what the vsize aggregate counted, not
// the folder (#2638), and still finds the one without a size.
func TestTheSearchForSizelessRecordsReadsNoFolder(t *testing.T) {
	cases := []struct {
		name     string
		sizeless int // which append carries no size; -1 none
	}{
		{name: "every append sized", sizeless: -1},
		{name: "one append without a size", sizeless: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var allocs [2]uint64
			for i, n := range []int{acceptSmall, acceptLarge} {
				alloc, got, want := sizelessAfterTail(t, n, tc.sizeless)
				allocs[i] = alloc
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%d: sizeless %v, want %v", n, got, want)
				}
			}
			if tc.sizeless >= 0 {
				return // a sizeless record is confirmed on the whole records
			}
			if grow := slope(allocs[0], allocs[1]); grow > acceptNoise {
				t.Errorf("%s", fmt.Sprintf("the search allocates %d B at %d, %d B at %d: %.1f B a message, bound %d",
					allocs[0], acceptSmall, allocs[1], acceptLarge, grow, acceptNoise))
			}
		})
	}
}

// A record without a size holds the aggregate until the driver fills it, and
// says so with the user, the folder and the uid: the cost it puts on every
// count is visible, not inferred.
func TestARecordWithoutASizeIsNamedInTheLog(t *testing.T) {
	home := movedFolder(t, 10)
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	var buf bytes.Buffer
	was := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(was)
	uid, err := h.AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, GUID: [16]byte{9}}); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	for _, want := range []string{"holds the folder's size aggregate", `"user":"` + testUser + `"`, `"folder":"INBOX"`, fmt.Sprintf(`"uid":%d`, uid)} {
		if !strings.Contains(line, want) {
			t.Errorf("the log says %q, want %s in it", line, want)
		}
	}
}
