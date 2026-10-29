package dboxindex

import "testing"

func intro(name string, width uint16) []byte {
	b := make([]byte, extIntroFixed)
	b[0] = 0xff // ext_id: a new extension, named below
	b[12], b[13] = byte(width), byte(width>>8)
	b[18], b[19] = byte(len(name)), byte(len(name)>>8)
	b = append(b, name...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// Refcounts one or four bytes wide, a position for an unappended uid and an
// expunge all read as the reference means; a refcount stops at zero.
func TestMapLogRecordsOfEveryWidthApply(t *testing.T) {
	for _, width := range []uint16{1, 4} {
		var log []byte
		delta := func(uid uint32, d int32) []byte {
			b := u32s(uid)
			switch width {
			case 1:
				b = append(b, byte(int8(d)), 0, 0, 0)
			case 4:
				b = append(b, u32s(uint32(d))...)
			}
			return b
		}
		for _, r := range [][]byte{
			frame(typeAppend, appended(1)),
			frame(typeExtIntro, intro("map", mapRecordSize)),
			frame(typeExtRecUpdate, append(u32s(2), u32s(7, 100, 50)...)),
			frame(typeExtIntro, intro("ref", width)),
			frame(typeExtAtomicInc, append(delta(1, 3), delta(1, -5)...)),
			frame(typeExtAtomicInc, delta(2, 2)),
			frame(typeExtAtomicInc, delta(9, 1)),
			frame(typeExpunge|expungeProt, u32s(1, 1)),
		} {
			log = append(log, r...)
		}
		entries, err := ReadMap(log, 0, nil)
		if err != nil {
			t.Fatalf("width %d: %v", width, err)
		}
		got := map[uint32]MapEntry{}
		for _, e := range entries {
			got[e.MapUID] = e
		}
		if len(got) != 2 {
			t.Errorf("width %d: %d entries, want uids 1 and 2", width, len(got))
		}
		if got[1].RefCount != 0 {
			t.Errorf("width %d: uid 1 refcount %d after +3 -5, want 0", width, got[1].RefCount)
		}
		if e := got[2]; e.FileID != 7 || e.Offset != 100 || e.Size != 50 || e.RefCount != 2 {
			t.Errorf("width %d: uid 2 %+v", width, e)
		}
	}
}

// Map log records that cannot mean what they say are refused.
func TestMalformedMapLogRecordsAreRefused(t *testing.T) {
	for name, log := range map[string][]byte{
		"intro shorter than its fixed part": frame(typeExtIntro, make([]byte, 8)),
		"intro naming bytes it lacks": func() []byte {
			b := intro("map", mapRecordSize)
			b[18] = 200
			return frame(typeExtIntro, b)
		}(),
		"map record narrower than a position": append(frame(typeExtIntro, intro("map", 4)), frame(typeExtRecUpdate, u32s(1, 7))...),
		"record past the file": func() []byte {
			b := frame(typeAppend, appended(1))
			return b[:len(b)-4]
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadMap(log, 0, nil); err == nil {
				t.Error("accepted")
			}
		})
	}
}
