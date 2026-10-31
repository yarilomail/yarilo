package mailindex

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// Malformed bytes in a journal header, an extension table or a transaction
// frame are refused with the error naming what is wrong, never decoded.
func TestMalformedWireBytesAreRefused(t *testing.T) {
	log := NewLogHeader(1, 2, 3)
	logBytes := log.EncodeBytes()
	exts, err := EncodeExtHeaders([]Extension{{Name: "modseq", HdrSize: 16, HdrData: make([]byte, 16), RecordSize: 8, RecordAlign: 8}})
	if err != nil {
		t.Fatal(err)
	}
	withHdrSize := func(n uint16) []byte {
		b := append([]byte(nil), logBytes...)
		binary.LittleEndian.PutUint16(b[2:], n)
		return b
	}
	for name, c := range map[string]struct {
		decode func() error
		want   error
	}{
		"journal header cut":       {func() error { _, err := DecodeLogHeader(bytes.NewReader(logBytes[:3])); return err }, ErrShortRead},
		"journal header too small": {func() error { _, err := DecodeLogHeader(bytes.NewReader(withHdrSize(8))); return err }, ErrCorrupted},
		"journal header rest cut": {func() error {
			_, err := DecodeLogHeader(bytes.NewReader(withHdrSize(LogHeaderSize)[:LogHeaderSize-4]))
			return err
		}, nil},
		"extension entry cut":  {func() error { _, err := DecodeExtHeaders(exts[:10]); return err }, ErrShortRead},
		"extension name cut":   {func() error { _, err := DecodeExtHeaders(exts[:18]); return err }, ErrShortRead},
		"extension data cut":   {func() error { _, err := DecodeExtHeaders(exts[:len(exts)-12]); return err }, ErrShortRead},
		"frame shorter than 8": {func() error { _, err := DecodeTxHeader(make([]byte, 4)); return err }, ErrShortRead},
		"frame size below 8": {func() error {
			b := make([]byte, 8)
			if err := EncodeTxHeader(b, TxHeader{Size: 8, Type: TxTypeFlags(TxTypeAppend)}); err != nil {
				return err
			}
			framed, _ := EncodeFramedSize(4)
			binary.LittleEndian.PutUint32(b[0:], framed)
			_, err := DecodeTxHeader(b)
			return err
		}, ErrCorrupted},
		"frame into a short buffer": {func() error { return EncodeTxHeader(make([]byte, 4), TxHeader{Size: 8}) }, ErrShortRead},
		"journal header claiming too little": {func() error {
			h := NewLogHeader(1, 2, 3)
			h.HdrSize = 8
			return h.Encode(&bytes.Buffer{})
		}, ErrCorrupted},
		"journal header not little-endian": {func() error {
			h := NewLogHeader(1, 2, 3)
			h.CompatFlags = 0
			return h.Encode(&bytes.Buffer{})
		}, ErrEndian},
	} {
		t.Run(name, func(t *testing.T) {
			err := c.decode()
			if err == nil {
				t.Fatal("accepted")
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("err %v, want %v", err, c.want)
			}
		})
	}
}

// A journal header from an older writer, shorter than ours but no shorter than
// the minimum, reads with the missing tail as zeros.
func TestAnOlderShorterJournalHeaderReads(t *testing.T) {
	old := NewLogHeader(7, 8, 9)
	b := old.EncodeBytes()
	binary.LittleEndian.PutUint16(b[2:], LogHeaderMinSize)
	h, err := DecodeLogHeader(bytes.NewReader(b[:LogHeaderMinSize]))
	if err != nil {
		t.Fatal(err)
	}
	if h.IndexID != 7 || h.FileSeq != 8 || h.HdrSize != LogHeaderMinSize {
		t.Errorf("read %+v", h)
	}
}
