package file

import (
	"container/list"
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
	// idle lists the cached images nobody references, oldest first, and
	// idleBytes is what they hold: a last close costs the same whatever the
	// number of images in use.
	idle      list.List
	idleBytes int64
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
	if old := c.m[path]; old != nil && old != img {
		c.unidleLocked(old)
	}
	c.m[path] = img
}

// Delete forgets the image at path, referenced or not.
func (c *imageCache) Delete(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(path)
}

func (c *imageCache) dropLocked(path string) {
	if img := c.m[path]; img != nil {
		c.unidleLocked(img)
		delete(c.m, path)
	}
}

// clearAll forgets every image.
func (c *imageCache) clearAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for path := range c.m {
		c.dropLocked(path)
	}
}

// unidleLocked takes img off the idle list, if it is there.
func (c *imageCache) unidleLocked(img *baseImage) {
	if img.idleElem == nil {
		return
	}
	c.idle.Remove(img.idleElem)
	c.idleBytes -= img.idleSize
	img.idleElem, img.idleSize = nil, 0
}

func (c *imageCache) refLocked(img *baseImage) {
	img.refs++
	c.unidleLocked(img)
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
	c.refLocked(img)
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
	c.refLocked(img)
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
	if c.m[img.path] != img {
		return // replaced or forgotten: nothing keeps it
	}
	img.idleSize = img.bytes()
	img.idleElem = c.idle.PushBack(img)
	c.idleBytes += img.idleSize
	c.after(imageIdleTimeout, c.sweep)
	c.trimLocked()
}

// sweep drops the idle images whose time is up.
func (c *imageCache) sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	for e := c.idle.Front(); e != nil; e = c.idle.Front() {
		img, _ := e.Value.(*baseImage)
		if now.Sub(img.idleAt) < imageIdleTimeout {
			return
		}
		c.dropLocked(img.path)
	}
}

// trimLocked drops idle images, oldest first, until they fit the budget.
func (c *imageCache) trimLocked() {
	budget := imageIdleBudget.Load()
	for e := c.idle.Front(); e != nil && c.idleBytes > budget; e = c.idle.Front() {
		img, _ := e.Value.(*baseImage)
		c.dropLocked(img.path)
	}
}

// bytes is what the image holds in records loaded so far.
func (img *baseImage) bytes() int64 {
	if img.recs == nil {
		return 0
	}
	return img.recs.LoadedBytes()
}
