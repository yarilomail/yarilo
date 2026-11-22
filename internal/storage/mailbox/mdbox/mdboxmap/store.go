package mdboxmap

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync/atomic"
)

// store is the map as a reader sees it: the base's record area left where it
// is, sorted by map_uid and read by position, under a small layer holding what
// the log and this handle's own writes changed since that base. A delivery
// touches the layer alone; nothing here holds the user's records in the heap.
type store struct {
	base *baseRecords
	// over shadows base by map_uid: a record added or changed since the base,
	// or gone from it. Bounded by the log, which a fold empties.
	over  map[uint32]overEntry
	count int
}

type overEntry struct {
	e    MapEntry
	gone bool
}

// baseReads counts positioned reads of a base and fullScans the passes over
// every record, so "a delivery reads nothing" and "a fetch walks nothing" are
// numbers a test reads rather than claims.
var baseReads, fullScans atomic.Int64

// BaseReads and FullScans return the counts; ResetCounts zeroes both.
func BaseReads() int { return int(baseReads.Load()) }
func FullScans() int { return int(fullScans.Load()) }
func ResetCounts()   { baseReads.Store(0); fullScans.Store(0) }
func noteFullScan()  { fullScans.Add(1) }
func noteBaseRead()  { baseReads.Add(1) }

// blockRecs is how many records one read brings in when the span left is that
// small: about 4 KiB, a page.
const blockRecs = 113

// baseRecords is a record area read in place: a v2 base on disk, or a v1 base
// decoded into memory. Shared by the handle and by any walk still running over
// it, so the descriptor closes on the last release.
type baseRecords struct {
	r    io.ReaderAt
	f    *os.File
	off  int64
	n    int
	next uint32 // the base's NextMapUID: no record at or above it is in the area
	// digest is the header's, checked at the end of every whole walk; zero for
	// an area decoded in memory, which has no header to keep honest.
	digest  uint64
	checked bool
	refs    atomic.Int32
}

func (b *baseRecords) ref() *baseRecords {
	if b != nil {
		b.refs.Add(1)
	}
	return b
}

func (b *baseRecords) release() {
	if b != nil && b.refs.Add(-1) == 0 && b.f != nil {
		b.f.Close() //nolint:errcheck
	}
}

// errBaseRead marks a positioned read of the base that failed or came back
// short: on NFS a replaced file answers ESTALE, and a short read with no error
// would otherwise be taken as records.
var errBaseRead = errors.New("mdboxmap: base read")

// readSeam is a test seam wrapping every base read. Nil elsewhere.
var readSeam func(b *baseRecords) io.ReaderAt

func (b *baseRecords) readAt(p []byte, i int) error {
	noteBaseRead()
	r := b.r
	if readSeam != nil {
		r = readSeam(b)
	}
	n, err := r.ReadAt(p, b.off+int64(i)*baseRecordLen)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("%w: %w", errBaseRead, err)
}

func (b *baseRecords) uidAt(i int) (uint32, error) {
	var buf [4]byte
	if err := b.readAt(buf[:], i); err != nil {
		return 0, err
	}
	return recordUID(buf[:]), nil
}

// span narrows [lo, n) to at most blockRecs records that hold uid if anything
// does, by probing map_uids alone.
func (b *baseRecords) span(lo int, uid uint32) (int, int, error) {
	hi := b.n
	for hi-lo > blockRecs {
		mid := int(uint(lo+hi) >> 1)
		u, err := b.uidAt(mid)
		if err != nil {
			return 0, 0, err
		}
		if u < uid {
			lo = mid + 1
		} else {
			hi = mid + 1
		}
	}
	return lo, hi, nil
}

// block reads records [lo, hi) in one read, into buf when it is large enough.
func (b *baseRecords) block(buf []byte, lo, hi int) ([]byte, error) {
	n := (hi - lo) * baseRecordLen
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if n == 0 {
		return buf, nil
	}
	return buf, b.readAt(buf, lo)
}

func (b *baseRecords) find(uid uint32) (MapEntry, bool, error) {
	if b == nil || b.n == 0 || uid >= b.next {
		return MapEntry{}, false, nil
	}
	lo, hi, err := b.span(0, uid)
	if err != nil {
		return MapEntry{}, false, err
	}
	blk, err := b.block(nil, lo, hi)
	if err != nil {
		return MapEntry{}, false, err
	}
	if i, ok := searchBlock(blk, uid); ok {
		return getRecord(blk[i*baseRecordLen:]), true, nil
	}
	return MapEntry{}, false, nil
}

func searchBlock(blk []byte, uid uint32) (int, bool) {
	n := len(blk) / baseRecordLen
	i := sort.Search(n, func(i int) bool { return recordUID(blk[i*baseRecordLen:]) >= uid })
	return i, i < n && recordUID(blk[i*baseRecordLen:]) == uid
}

// each walks the area in order, a block at a time, and checks the digest once
// a walk has seen all of it.
func (b *baseRecords) each(fn func(MapEntry) error) error {
	if b == nil || b.n == 0 {
		return nil
	}
	const chunk = 16 * blockRecs
	h := newDigest()
	var blk []byte
	for lo := 0; lo < b.n; lo += chunk {
		hi := min(lo+chunk, b.n)
		var err error
		blk, err = b.block(blk, lo, hi)
		if err != nil {
			return err
		}
		h.write(blk)
		for i := 0; i < hi-lo; i++ {
			if err := fn(getRecord(blk[i*baseRecordLen:])); err != nil {
				return err
			}
		}
	}
	if b.checked && h.sum() != b.digest {
		return fmt.Errorf("mdboxmap: base records do not hash to the header's digest")
	}
	return nil
}

// get resolves uid through the layer, then the base.
func (s *store) get(uid uint32) (MapEntry, bool, error) {
	if o, ok := s.over[uid]; ok {
		return o.e, !o.gone, nil
	}
	return s.base.find(uid)
}

// put adds or replaces a record.
func (s *store) put(e MapEntry) error {
	_, had, err := s.get(e.UID)
	if err != nil {
		return err
	}
	if !had {
		s.count++
	}
	if s.over == nil {
		s.over = map[uint32]overEntry{}
	}
	s.over[e.UID] = overEntry{e: e}
	return nil
}

// remove drops uid, reporting whether it was there.
func (s *store) remove(uid uint32) (bool, error) {
	_, had, err := s.get(uid)
	if err != nil || !had {
		return false, err
	}
	if s.over == nil {
		s.over = map[uint32]overEntry{}
	}
	s.over[uid] = overEntry{e: MapEntry{UID: uid}, gone: true}
	s.count--
	return true, nil
}

// snapshot is a walk's view: the base it holds a reference to and the layer
// copied, so the walk runs with the handle's mutex released.
type snapshot struct {
	base *baseRecords
	over []overEntry
}

func (s *store) snapshot() snapshot {
	over := make([]overEntry, 0, len(s.over))
	for _, o := range s.over {
		over = append(over, o)
	}
	sort.Slice(over, func(i, j int) bool { return over[i].e.UID < over[j].e.UID })
	return snapshot{base: s.base.ref(), over: over}
}

func (sn snapshot) release() { sn.base.release() }

// each merges base and layer in map_uid order.
func (sn snapshot) each(fn func(MapEntry) error) error {
	j := 0
	emitUpTo := func(uid uint32, inclusive bool) (bool, error) {
		shadowed := false
		for j < len(sn.over) && (sn.over[j].e.UID < uid || inclusive && sn.over[j].e.UID == uid) {
			o := sn.over[j]
			j++
			if o.e.UID == uid {
				shadowed = true
			}
			if !o.gone {
				if err := fn(o.e); err != nil {
					return shadowed, err
				}
			}
		}
		return shadowed, nil
	}
	err := sn.base.each(func(e MapEntry) error {
		shadowed, err := emitUpTo(e.UID, true)
		if err != nil || shadowed {
			return err
		}
		return fn(e)
	})
	if err != nil {
		return err
	}
	_, err = emitUpTo(^uint32(0), true)
	return err
}

// scan is a whole walk for a question, counted: the paths a delivery, a fetch
// and an expunge take must never come here.
func (s *store) scan(fn func(MapEntry) error) error {
	noteFullScan()
	sn := s.snapshot()
	defer sn.release()
	return sn.each(fn)
}

// lookupMany resolves uids in one forward pass: sorted, each looked for in the
// block the previous one brought in before any probe further on.
func (s *store) lookupMany(uids []uint32) (map[uint32]MapEntry, error) {
	out := make(map[uint32]MapEntry, len(uids))
	sorted := append([]uint32(nil), uids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	b := s.base
	var blk []byte
	end := 0 // records before end are behind the pass
	for k, uid := range sorted {
		if k > 0 && uid == sorted[k-1] {
			continue
		}
		if o, ok := s.over[uid]; ok {
			if !o.gone {
				out[uid] = o.e
			}
			continue
		}
		if b == nil || b.n == 0 || uid >= b.next {
			continue
		}
		if n := len(blk) / baseRecordLen; n > 0 && uid <= recordUID(blk[(n-1)*baseRecordLen:]) {
			if i, ok := searchBlock(blk, uid); ok {
				out[uid] = getRecord(blk[i*baseRecordLen:])
			}
			continue
		}
		if end >= b.n {
			continue
		}
		// The next block first: a folder's map_uids mostly come in runs.
		next, err := b.block(blk, end, min(end+blockRecs, b.n))
		if err != nil {
			return nil, err
		}
		if n := len(next) / baseRecordLen; uid > recordUID(next[(n-1)*baseRecordLen:]) && end+n < b.n {
			lo, _, err := b.span(end+n, uid)
			if err != nil {
				return nil, err
			}
			if next, err = b.block(next, lo, min(lo+blockRecs, b.n)); err != nil {
				return nil, err
			}
			end = lo
		}
		blk = next
		end += len(blk) / baseRecordLen
		if i, ok := searchBlock(blk, uid); ok {
			out[uid] = getRecord(blk[i*baseRecordLen:])
		}
	}
	return out, nil
}

// digest is FNV-1a over the record area, fed as it streams: a change detector
// for a file we read, not a defence against a forged one.
type digest uint64

func newDigest() *digest { d := digest(14695981039346656037); return &d }

func (d *digest) write(p []byte) {
	h := uint64(*d)
	for _, c := range p {
		h ^= uint64(c)
		h *= 1099511628211
	}
	*d = digest(h)
}

func (d *digest) sum() uint64 { return uint64(*d) }

// memBase is an area decoded in memory, from a v1 base or a test.
func memBase(entries []MapEntry, next uint32) *baseRecords {
	buf := make([]byte, len(entries)*baseRecordLen)
	for i, e := range entries {
		putRecord(buf[i*baseRecordLen:], e)
	}
	b := &baseRecords{r: bytes.NewReader(buf), n: len(entries), next: next}
	b.refs.Store(1)
	return b
}

// writeRecords streams sn through keep into w as fixed-width records, and
// reports how many and their digest. keep may change the record; false drops it.
func writeRecords(w io.Writer, sn snapshot, keep func(*MapEntry) bool) (int, uint64, error) {
	bw := bufio.NewWriterSize(w, 64<<10)
	h := newDigest()
	var rec [baseRecordLen]byte
	n := 0
	var cur MapEntry // one variable: keep(&e) would move every record to the heap
	err := sn.each(func(e MapEntry) error {
		cur = e
		if keep != nil && !keep(&cur) {
			return nil
		}
		putRecord(rec[:], cur)
		h.write(rec[:])
		n++
		_, err := bw.Write(rec[:])
		return err
	})
	if err == nil {
		err = bw.Flush()
	}
	return n, h.sum(), err
}
