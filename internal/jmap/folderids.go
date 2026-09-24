package jmap

import "sync"

// folderIdentities remembers which folder wears which GUID. A copy in the GUID
// store names its folder by identity, and the folder list carries none, so
// without this every id lookup opens folders until it matches (#1711).
type folderIdentities struct {
	mu     sync.Mutex
	byGUID map[[16]byte]string
}

func (f *folderIdentities) name(guid [16]byte) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.byGUID[guid]
	return name, ok
}

func (f *folderIdentities) remember(guid [16]byte, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byGUID == nil {
		f.byGUID = map[[16]byte]string{}
	}
	f.byGUID[guid] = name
}

// forget drops one identity, for a name that no longer wears it: a folder
// deleted and recreated keeps the name and changes the GUID.
func (f *folderIdentities) forget(guid [16]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byGUID, guid)
}

// folderIdentitiesFor returns the map for one user, shared by every request
// this process serves for them.
func (s *Storage) folderIdentitiesFor(username string) *folderIdentities {
	s.folderIDsMu.Lock()
	defer s.folderIDsMu.Unlock()
	if s.folderIDs == nil {
		s.folderIDs = map[string]*folderIdentities{}
	}
	f, ok := s.folderIDs[username]
	if !ok {
		f = &folderIdentities{}
		s.folderIDs[username] = f
	}
	return f
}
