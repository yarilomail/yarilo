package dboxindex

import (
	"encoding/binary"
	"slices"
	"testing"
)

// frame is one log record as the reference writes it: a size whose every byte
// carries its high bit, the type, then the data.
func frame(recType uint32, data []byte) []byte {
	size := uint32(8 + len(data))
	v := size >> 2
	packed := 0x80808080 | v&0x7f | (v>>7&0x7f)<<8 | (v>>14&0x7f)<<16 | (v>>21&0x7f)<<24
	out := make([]byte, 8, size)
	binary.BigEndian.PutUint32(out, packed)
	binary.LittleEndian.PutUint32(out[4:], recType)
	return append(out, data...)
}

func u32s(vs ...uint32) []byte {
	out := make([]byte, 0, 4*len(vs))
	for _, v := range vs {
		out = binary.LittleEndian.AppendUint32(out, v)
	}
	return out
}

func keywordUpdate(modify byte, name string, first, last uint32) []byte {
	b := []byte{modify, 0, byte(len(name)), byte(len(name) >> 8)}
	b = append(b, name...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return append(b, u32s(first, last)...)
}

func appended(uids ...uint32) []byte {
	var b []byte
	for _, u := range uids {
		b = append(b, u32s(u, 0)...)
	}
	return b
}

// Keyword removals, keyword resets, a modseq-only flag update and the older
// expunge record each change the mailbox as the reference meant them to.
func TestOlderAndRarerLogRecordsApply(t *testing.T) {
	var log []byte
	for _, r := range [][]byte{
		frame(typeAppend, appended(1, 2, 3)),
		frame(typeKeywordUpdate, keywordUpdate(modifyAdd, "$A", 1, 2)),
		frame(typeKeywordUpdate, keywordUpdate(modifyAdd, "$B", 1, 2)),
		frame(typeKeywordUpdate, keywordUpdate(modifyRemove, "$A", 2, 2)),
		frame(typeKeywordReset, u32s(1, 1)),
		frame(typeFlagUpdate, append(u32s(1, 3), 0, 0, 0, 0)),
		frame(typeExpunge|expungeProt, u32s(3, 3)),
	} {
		log = append(log, r...)
	}
	changes, err := ReadChanges(log, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Type == FlagsChanged {
			t.Errorf("a modseq-only update read as a flag change: %+v", c)
		}
	}
	recs := Apply(nil, changes, nil)
	byUID := map[uint32]Record{}
	for _, r := range recs {
		byUID[r.UID] = r
	}
	if _, have := byUID[3]; have {
		t.Error("uid 3 survived the older expunge record")
	}
	if kw := byUID[1].Keywords; len(kw) != 0 {
		t.Errorf("uid 1 keywords %v after the reset, want none", kw)
	}
	if kw := byUID[2].Keywords; !slices.Equal(kw, []string{"$B"}) {
		t.Errorf("uid 2 keywords %v, want [$B]", kw)
	}
}

// A record that cannot mean what it says is refused, not guessed at.
func TestMalformedLogRecordsAreRefused(t *testing.T) {
	for name, log := range map[string][]byte{
		"keyword replace":               frame(typeKeywordUpdate, keywordUpdate(2, "$A", 1, 1)),
		"keyword name too long":         frame(typeKeywordUpdate, []byte{modifyAdd, 0, 200, 0, '$', 'A', 0, 0}),
		"keyword range reversed":        frame(typeKeywordUpdate, keywordUpdate(modifyAdd, "$A", 5, 1)),
		"reset range reversed":          frame(typeKeywordReset, u32s(5, 1)),
		"expunge range reversed":        frame(typeExpunge|expungeProt, u32s(5, 1)),
		"flag range reversed":           frame(typeFlagUpdate, append(u32s(5, 1), 1, 0, 0, 0)),
		"header update past the record": frame(typeHeaderUpdate, []byte{0, 0, 40, 0, 1, 2}),
		"record past the file": func() []byte {
			b := frame(typeAppend, appended(1))
			return b[:len(b)-4]
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadChanges(log, 0, nil); err == nil {
				t.Error("accepted")
			}
		})
	}
	if _, err := ParseLogHeader(make([]byte, 10)); err == nil {
		t.Error("a 10-byte log header was accepted")
	}
}
