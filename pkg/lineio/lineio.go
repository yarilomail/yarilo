// Package lineio reads LF-terminated lines without holding more than a bound.
package lineio

import (
	"bufio"
	"errors"
)

// MaxClient bounds a line a client sends; MaxInternal one between yarilo's own
// services, whose lines carry data (userdb fields, dict values, search hits).
const (
	MaxClient   = 64 << 10
	MaxInternal = 1 << 20
)

// ErrTooLong is a line that passed its bound; the rest of it was not read.
var ErrTooLong = errors.New("lineio: line too long")

// ReadLine is ReadString('\n') with a bound: past max bytes it stops and
// returns ErrTooLong with the first max bytes, having held no more.
func ReadLine(r *bufio.Reader, max int) (string, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			return string(append(buf, chunk[:max-len(buf)]...)), ErrTooLong
		}
		buf = append(buf, chunk...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return string(buf), err
		}
	}
}
