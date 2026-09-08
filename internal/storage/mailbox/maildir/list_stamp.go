package maildir

import (
	"os"
	"syscall"
	"time"
)

// listStamp is the list file's identity as the reference reads it: a rewrite
// renames, so the inode moves even when size and mtime collide (#1739).
type listStamp struct {
	ino   uint64
	size  int64
	mtime time.Time
}

// stampOf reads that identity. A filesystem that will not say gives inode 0,
// which still compares as itself and leaves size and mtime doing the work.
func stampOf(fi os.FileInfo) listStamp {
	s := listStamp{size: fi.Size(), mtime: fi.ModTime()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		s.ino = st.Ino
	}
	return s
}

// same reports whether two stamps name the same file in the same state.
func (s listStamp) same(other listStamp) bool {
	return s.ino == other.ino && s.size == other.size && s.mtime.Equal(other.mtime)
}
