package file

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// An image a folder state references stays whatever the budget; one nobody
// references leaves after imageIdleTimeout on the stand's clock, or at once
// over the budget.
func TestABaseImageLivesWhileReferencedAndForItsIdleTime(t *testing.T) {
	cases := []struct {
		name   string
		budget int64
		// idle is how long after the close the sweep runs; held what it finds.
		idle     time.Duration
		heldOpen bool
		heldIdle bool
		// extends are appends another handle makes, each read after: the
		// published view is extended, and every one it replaced lets go.
		extends int
	}{
		{name: "referenced over a zero budget", budget: 0, heldOpen: true, heldIdle: false},
		{name: "idle within its time", budget: 1 << 30, idle: imageIdleTimeout - time.Second, heldOpen: true, heldIdle: true},
		{name: "idle past its time", budget: 1 << 30, idle: imageIdleTimeout, heldOpen: true, heldIdle: false},
		{name: "idle past its time after extended views", budget: 1 << 30, idle: imageIdleTimeout, heldOpen: true, heldIdle: false, extends: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withIdleBudget(t, tc.budget)
			now := time.Unix(1700000000, 0)
			var sweeps []func()
			c := baseImages
			c.mu.Lock()
			wasClock, wasAfter := c.clock, c.after
			c.clock = func() time.Time { return now }
			c.after = func(_ time.Duration, f func()) { sweeps = append(sweeps, f) }
			c.mu.Unlock()
			t.Cleanup(func() {
				c.mu.Lock()
				c.clock, c.after = wasClock, wasAfter
				c.mu.Unlock()
			})

			home := acceptFolder(t, 300)
			path := indexPathFor(home)
			baseImages.Delete(path)
			h, f := acceptOpen(t, home)
			if _, err := h.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
				t.Fatal(err)
			}
			if tc.extends > 0 {
				w, wf := acceptOpen(t, home)
				for i := 0; i < tc.extends; i++ {
					uid, _ := w.AllocateUID(wf.ID)
					if err := w.AppendMessage(wf.ID, &mailbox.MessageMeta{UID: uid, GUID: [16]byte{9, byte(i)}, Size: 10, VSize: 10}); err != nil {
						t.Fatal(err)
					}
					if _, err := h.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
						t.Fatal(err)
					}
				}
				w.Close() //nolint:errcheck
			}
			// Another folder going idle trims the cache with this one in use.
			other := acceptFolder(t, 300)
			oh, of := acceptOpen(t, other)
			if _, err := oh.GetMessages(of.ID, mailbox.SeqSet{}); err != nil {
				t.Fatal(err)
			}
			oh.Close() //nolint:errcheck
			if got := baseImages.held(path); got != tc.heldOpen {
				t.Fatalf("with the folder open the image is held %v, want %v", got, tc.heldOpen)
			}
			h.Close() //nolint:errcheck
			c.mu.Lock()
			now = now.Add(tc.idle)
			c.mu.Unlock()
			for _, sweep := range sweeps {
				sweep()
			}
			if got := baseImages.held(path); got != tc.heldIdle {
				t.Fatalf("%v after the close the image is held %v, want %v", tc.idle, got, tc.heldIdle)
			}
		})
	}
}

// A view a reader still holds references its image after the folder state let
// go of it: over a zero budget the image stays until the view is released.
func TestAHeldViewKeepsItsImage(t *testing.T) {
	withIdleBudget(t, 0)
	home := acceptFolder(t, 300)
	path := indexPathFor(home)
	baseImages.Delete(path)
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	fs, err := h.(*userHandle).ui.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := fs.openView()
	if err != nil {
		t.Fatal(err)
	}
	fs.unpinImage()
	if !baseImages.held(path) {
		t.Fatal("the image went with a view still held")
	}
	release()
	if baseImages.held(path) {
		t.Error("the image stayed after its last view was released")
	}
}

// closeCost is what one image's last close takes beside n images in use,
// per close, the median of five rounds.
func closeCost(n int) time.Duration {
	c := &imageCache{m: map[string]*baseImage{}, clock: time.Now, after: func(time.Duration, func()) {}}
	for i := 0; i < n; i++ {
		img := &baseImage{refs: 1, path: fmt.Sprint(i)}
		c.m[img.path] = img
	}
	img := &baseImage{path: "closed"}
	c.m[img.path] = img
	var rounds []time.Duration
	for r := 0; r < 5; r++ {
		start := time.Now()
		for i := 0; i < 1000; i++ {
			c.ref(img)
			c.unref(img)
		}
		rounds = append(rounds, time.Since(start)/1000)
	}
	slices.Sort(rounds)
	return rounds[2]
}

// A last close does not walk the images in use: beside 100 and beside 100000
// it costs the same, where a walk of the cache costs a thousand times more.
func TestALastCloseDoesNotWalkTheCache(t *testing.T) {
	small, large := closeCost(100), closeCost(100000)
	if large > 5*small+time.Microsecond {
		t.Errorf("a last close takes %v beside 100 images in use, %v beside 100000", small, large)
	}
}
