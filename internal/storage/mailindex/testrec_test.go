package mailindex

// testRec is a record spelled out for a test, encoded through the layout.
type testRec struct {
	UID   uint32
	Flags MailFlag
	Ext   map[string][]byte
}

func encodeTestRecs(l RecordLayout, recs []testRec) []byte {
	out := make([]byte, 0, len(recs)*int(l.RecordSize))
	for _, r := range recs {
		rec := l.NewRecord(r.UID, r.Flags)
		for name, data := range r.Ext {
			if err := l.SetExt(rec, name, data); err != nil {
				panic(err)
			}
		}
		out = append(out, rec...)
	}
	return out
}

func testChunks(l RecordLayout, recs []testRec) (*Records, error) {
	return ChunksFromBytes(encodeTestRecs(l, recs), l)
}

// setTestRecs gives f the records, in its layout.
func setTestRecs(f *File, recs []testRec) {
	r, err := testChunks(f.Layout, recs)
	if err != nil {
		panic(err)
	}
	f.Records = r
}
