package file

// IndexDirFor is where this folder's index lives: the token cache is keyed by
// it, because one user's namespaces resolve to different directories.
func (u *userIndex) IndexDirFor(folder string) string { return u.indexDir(folder) }
