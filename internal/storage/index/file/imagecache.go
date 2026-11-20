package file

import (
	"sync"
	"sync/atomic"
	"time"
)

// imageIdleTimeout is how long a base image nobody references stays for the
// next open of the same folder (INDEX-MEMORY.md §5).
const imageIdleTimeout = 10 * time.Second

// defaultImageIdleBudget is what images nobody references may hold together.
const defaultImageIdleBudget = 64 << 20

// imageIdleBudget is shared by every Backend of the process, as the cache is.
var imageIdleBudget atomic.Int64

// imageCache holds one image per base path. A folder state references the
// image it last read; an image with no reference is idle and leaves after
// imageIdleTimeout, or at once, oldest first, while the idle ones exceed the
// budget.
type imageCache struct {
	mu sync.Mutex
	m  map[string]*baseImage
	// clock and after are test seams under mu: the clock idle time is read
	// from, and the timer that sweeps after imageIdleTimeout.
	clock func() time.Time
	after func(time.Duration, func())
}

func newImageCache() *imageCache {
	imageIdleBudget.Store(defaultImageIdleBudget)
	return &imageCache{
		m:     make(map[string]*baseImage),
		clock: time.Now,
		after: func(d time.Duration, f func()) { time.AfterFunc(d, f) },
	}
}

func (c *imageCache) load(path string) *baseImage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[path]
}

func (c *imageCache) store(path string, img *baseImage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	img.path = path
	c.m[path] = img
}

// Delete forgets the image at path, referenced or not.
func (c *imageCache) Delete(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, path)
}

// held reports whether an image for path is cached, for tests.
func (c *imageCache) held(path string) bool { return c.load(path) != nil }

// pinImage moves fs's reference to img, idling the image it held before.
func (fs *folderState) pinImage(img *baseImage) {
	c := baseImages
	c.mu.Lock()
	defer c.mu.Unlock()
	if fs.pinned == img {
		return
	}
	img.refs++
	old := fs.pinned
	fs.pinned = img
	c.unrefLocked(old)
}

// unpinImage drops fs's reference and its published view's, for a folder
// state that is going away.
func (fs *folderState) unpinImage() {
	fs.mapMu.Lock()
	if m := fs.current; m != nil {
		fs.current = nil
		fs.retireLocked(m)
	}
	fs.mapMu.Unlock()
	c := baseImages
	c.mu.Lock()
	defer c.mu.Unlock()
	old := fs.pinned
	fs.pinned = nil
	c.unrefLocked(old)
}

// ref and unref are a view's reference to the image it was folded from.
func (c *imageCache) ref(img *baseImage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	img.refs++
}

func (c *imageCache) unref(img *baseImage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unrefLocked(img)
}

func (c *imageCache) unrefLocked(img *baseImage) {
	if img == nil {
		return
	}
	img.refs--
	if img.refs > 0 {
		return
	}
	img.idleAt = c.clock()
	if c.m[img.path] == img {
		c.after(imageIdleTimeout, c.sweep)
	}
	c.trimLocked()
}

// sweep drops the idle images whose time is up.
func (c *imageCache) sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	for path, img := range c.m {
		if img.refs == 0 && now.Sub(img.idleAt) >= imageIdleTimeout {
			delete(c.m, path)
		}
	}
}

// trimLocked drops idle images, oldest first, until they fit the budget.
func (c *imageCache) trimLocked() {
	budget := imageIdleBudget.Load()
	for {
		var total int64
		var oldest *baseImage
		for _, img := range c.m {
			if img.refs > 0 {
				continue
			}
			total += img.bytes()
			if oldest == nil || img.idleAt.Before(oldest.idleAt) {
				oldest = img
			}
		}
		if oldest == nil || total <= budget {
			return
		}
		delete(c.m, oldest.path)
	}
}

// bytes is what the image holds in records loaded so far.
func (img *baseImage) bytes() int64 {
	if img.recs == nil {
		return 0
	}
	return img.recs.LoadedBytes()
}
