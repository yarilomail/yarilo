package dboxconv_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxconv"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxindex"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
)

// transactionStarts returns where each transaction but the first starts: a
// boundary record spans a multi-record one, any other record is one alone.
func transactionStarts(t *testing.T, log []byte) []int {
	t.Helper()
	const boundary = 0x00080000
	var starts []int
	hdr := int(binary.LittleEndian.Uint16(log[2:]))
	txEnd := hdr
	for pos := hdr; pos+8 <= len(log); {
		be := binary.BigEndian.Uint32(log[pos:])
		size := int(((be & 0x7f) | (be&0x7f00)>>8<<7 | (be&0x7f0000)>>16<<14 | (be&0x7f000000)>>24<<21) << 2)
		if size < 8 || pos+size > len(log) {
			t.Fatalf("record at %d: size %d", pos, size)
		}
		if pos >= txEnd {
			if pos > hdr {
				starts = append(starts, pos)
			}
			txEnd = pos + size
			if binary.LittleEndian.Uint32(log[pos+4:])&0x0fffffff == boundary {
				txEnd = pos + int(binary.LittleEndian.Uint32(log[pos+8:]))
			}
		}
		pos += size
	}
	if len(starts) == 0 {
		t.Fatal("one transaction, nowhere to cut; the fixture proves nothing")
	}
	return starts
}

// rotate splits a log at cut the way a rotation leaves it: the older file as it
// was, the newer one a copy of its header naming the older as its predecessor.
func rotate(log []byte, cut int) (older, newer []byte) {
	hdr := int(binary.LittleEndian.Uint16(log[2:]))
	seq := binary.LittleEndian.Uint32(log[8:])
	newer = append(append([]byte{}, log[:hdr]...), log[cut:]...)
	binary.LittleEndian.PutUint32(newer[8:], seq+1)
	binary.LittleEndian.PutUint32(newer[12:], seq)
	binary.LittleEndian.PutUint32(newer[16:], uint32(cut))
	return log[:cut], newer
}

// continuing makes a log claim a predecessor that is not on disk.
func continuing(log []byte) []byte {
	out := append([]byte{}, log...)
	seq := binary.LittleEndian.Uint32(out[8:])
	binary.LittleEndian.PutUint32(out[8:], seq+1)
	binary.LittleEndian.PutUint32(out[12:], seq)
	return out
}

func writeFiles(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type place struct{ file, offset, size, refs uint32 }

func placesOf(t *testing.T, entries []dboxindex.MapEntry) []place {
	t.Helper()
	out := make([]place, 0, len(entries))
	for _, e := range entries {
		out = append(out, place{e.FileID, e.Offset, e.Size, uint32(e.RefCount)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].offset < out[j].offset
	})
	return out
}

// The field's store without its base: a bare current log and the history in the
// rotated one must read as that base read alone (#2253).
func TestAMapWithoutABaseIsReadFromItsLogChain(t *testing.T) {
	baseOnly, err := dboxconv.ReadForeignMap(writeFiles(t, map[string][]byte{
		"dovecot.map.index":     dboxref.MapRotatedBase(t),
		"dovecot.map.index.log": dboxref.MapRotatedLog(t),
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := placesOf(t, baseOnly)
	for _, p := range want {
		if p.refs == 0 {
			t.Fatalf("%+v reads as unreferenced on a store that has expunged nothing", p)
		}
	}
	if len(want) != 3020 {
		t.Fatalf("the base reads %d records, and the reference counted 3020 messages", len(want))
	}

	got, err := dboxconv.ReadForeignMap(writeFiles(t, map[string][]byte{
		"dovecot.map.index.log":   dboxref.MapRotatedLog(t),
		"dovecot.map.index.log.2": dboxref.MapRotatedPrev(t),
	}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if g := placesOf(t, got); len(g) != len(want) {
		t.Fatalf("the logs give %d records and the base %d", len(g), len(want))
	} else {
		for i := range g {
			if g[i] != want[i] {
				t.Fatalf("record %d: the logs point at %+v, the base at %+v", i, g[i], want[i])
			}
		}
	}
}

// A rotation may fall between any two transactions, and the newer file names an
// extension by the id the older one gave it. Every cut must read as the whole.
func TestARotationAtEveryRecordReadsAsTheWholeLog(t *testing.T) {
	whole := dboxref.MapLog(t)
	want, err := dboxindex.ReadMap(whole, int(binary.LittleEndian.Uint16(whole[2:])), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range transactionStarts(t, whole) {
		older, newer := rotate(whole, cut)
		got, err := dboxconv.ReadForeignMap(writeFiles(t, map[string][]byte{
			"dovecot.map.index.log":   newer,
			"dovecot.map.index.log.2": older,
		}))
		if err != nil {
			t.Fatalf("cut at %d: %v", cut, err)
		}
		w, g := placesOf(t, want), placesOf(t, got)
		if len(g) != len(w) {
			t.Fatalf("cut at %d: %d records, and the whole log has %d", cut, len(g), len(w))
		}
		for i := range g {
			if g[i] != w[i] {
				t.Fatalf("cut at %d: record %d at %+v, the whole log says %+v", cut, i, g[i], w[i])
			}
		}
	}

	// The folder log carries its own extensions the same way.
	fresh := dboxref.IndexFreshLog(t)
	wantFolder, err := dboxconv.ReadForeignFolder(writeFiles(t, map[string][]byte{"dovecot.index.log": fresh}))
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range transactionStarts(t, fresh) {
		older, newer := rotate(fresh, cut)
		got, err := dboxconv.ReadForeignFolder(writeFiles(t, map[string][]byte{
			"dovecot.index.log":   newer,
			"dovecot.index.log.2": older,
		}))
		if err != nil {
			t.Fatalf("folder cut at %d: %v", cut, err)
		}
		if len(got.Records) != len(wantFolder.Records) || got.Header != wantFolder.Header {
			t.Fatalf("folder cut at %d: %d records %+v, the whole log %d %+v",
				cut, len(got.Records), got.Header, len(wantFolder.Records), wantFolder.Header)
		}
		for i, r := range got.Records {
			mapExt, ok := dboxindex.Find(got.Exts, "mdbox")
			if !ok {
				t.Fatalf("folder cut at %d: no mdbox extension, so no message can be located", cut)
			}
			gf, _ := dboxindex.FieldOf(r, mapExt)
			wf, _ := dboxindex.FieldOf(wantFolder.Records[i], mapExt)
			if r.UID != wantFolder.Records[i].UID || string(gf) != string(wf) {
				t.Fatalf("folder cut at %d: uid %d map field %x, the whole log uid %d %x",
					cut, r.UID, gf, wantFolder.Records[i].UID, wf)
			}
		}
	}
}

// A base whose tail is in the rotated log reads on from there, then the whole new
// log; cut past the tail so the base misses changes on both sides of the cut.
func TestAFolderBaseReadsOnFromTheRotatedLog(t *testing.T) {
	base, whole := dboxref.IndexBase(t), dboxref.IndexLog(t)
	h, err := dboxindex.ParseHeader(base)
	if err != nil {
		t.Fatal(err)
	}
	want, err := dboxconv.ReadForeignFolder(writeFiles(t, map[string][]byte{
		"dovecot.index": base, "dovecot.index.log": whole,
	}))
	if err != nil {
		t.Fatal(err)
	}
	cuts := 0
	for _, cut := range transactionStarts(t, whole) {
		if cut <= int(h.LogFileTailOffset) {
			continue
		}
		cuts++
		older, newer := rotate(whole, cut)
		got, err := dboxconv.ReadForeignFolder(writeFiles(t, map[string][]byte{
			"dovecot.index": base, "dovecot.index.log": newer, "dovecot.index.log.2": older,
		}))
		if err != nil {
			t.Fatalf("cut at %d: %v", cut, err)
		}
		if len(got.Records) != len(want.Records) || got.Header != want.Header {
			t.Fatalf("cut at %d: %d records %+v, base and whole log %d %+v",
				cut, len(got.Records), got.Header, len(want.Records), want.Header)
		}
		for i, r := range got.Records {
			w := want.Records[i]
			if r.UID != w.UID || r.Flags != w.Flags || fmt.Sprint(r.Keywords) != fmt.Sprint(w.Keywords) {
				t.Fatalf("cut at %d: uid %d flags %#x %v, the whole log uid %d flags %#x %v",
					cut, r.UID, r.Flags, r.Keywords, w.UID, w.Flags, w.Keywords)
			}
		}
	}
	if cuts == 0 {
		t.Fatalf("no transaction past the base's tail at %d; the fixture proves nothing", h.LogFileTailOffset)
	}
}

// Logs that do not reach back to the start of the history are refused, not read
// as a short map: a short map is a folder whose messages point at nothing.
func TestALogChainShortOfItsStartIsRefused(t *testing.T) {
	whole := dboxref.MapLog(t)
	older, newer := rotate(whole, transactionStarts(t, whole)[1])
	based := dboxref.MapBaseLog(t)
	_, basedNext := rotate(based, transactionStarts(t, based)[1])
	hdr := int(binary.LittleEndian.Uint16(basedNext[2:]))
	_, folderNext := rotate(dboxref.IndexLog(t), transactionStarts(t, dboxref.IndexLog(t))[1])
	for _, tc := range []struct {
		name  string
		files map[string][]byte
	}{
		{"map: the current log continues one that is gone", map[string][]byte{
			"dovecot.map.index.log": newer,
		}},
		{"map: the rotated log continues one that is gone", map[string][]byte{
			"dovecot.map.index.log":   newer,
			"dovecot.map.index.log.2": continuing(older),
		}},
		{"map: the base's log was removed after the rotation", map[string][]byte{
			"dovecot.map.index":     dboxref.MapBase(t),
			"dovecot.map.index.log": basedNext,
		}},
		{"map: the base is two rotations behind", map[string][]byte{
			"dovecot.map.index":       dboxref.MapBase(t),
			"dovecot.map.index.log.2": basedNext,
			"dovecot.map.index.log":   continuing(basedNext[:hdr]),
		}},
		{"map: a log between the base's and the current one is gone", map[string][]byte{
			"dovecot.map.index":       dboxref.MapBase(t),
			"dovecot.map.index.log.2": based,
			"dovecot.map.index.log":   continuing(continuing(based[:hdr])),
		}},
		{"folder: the base's log was removed after the rotation", map[string][]byte{
			"dovecot.index":     dboxref.IndexBase(t),
			"dovecot.index.log": folderNext,
		}},
		{"folder: the field's rotated pair, whose older log continues a third", map[string][]byte{
			"dovecot.index.log":   dboxref.IndexLog(t),
			"dovecot.index.log.2": dboxref.IndexLogRotated(t),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, tc.files)
			var err error
			if _, ok := tc.files["dovecot.index.log"]; ok {
				_, err = dboxconv.ReadForeignFolder(dir)
			} else {
				_, err = dboxconv.ReadForeignMap(dir)
			}
			if !errors.Is(err, dboxconv.ErrHistoryLost) {
				t.Fatalf("read gave %v, want ErrHistoryLost", err)
			}
		})
	}
}
