package virtual

import (
	"fmt"
	"strings"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Backing is one folder a virtual mailbox draws from, as the caller resolves
// it: the session knows the namespaces, this package does not.
type Backing struct {
	Name string
	GUID [16]byte
	// The folder's state now: a pass compares it with what the header last
	// saw, and reads no message of a folder where nothing moved.
	UIDValidity   uint32
	NextUID       uint32
	HighestModSeq uint64
	Messages      []*mailbox.MessageMeta
}

// Resolver hands the sync the folders a configuration names. Patterns are
// expanded by the caller, which is where the namespace and its separator are.
type Resolver interface {
	// Folders returns every backing folder the configuration selects, after
	// the negative lines have taken theirs out, without their messages.
	Folders(cfg *Config) ([]Backing, error)
	// Messages reads a folder's messages, for a pass that has to look.
	Messages(b Backing) ([]*mailbox.MessageMeta, error)
	// Matches says which of the folder's messages the rule keeps. A rule that
	// is empty keeps them all.
	Matches(b Backing, rule string) (map[uint32]bool, error)
}

// Mode is when a pass runs, which decides what a record that stopped matching
// its rule does (virtual-sync.c:2012-2017).
type Mode int

const (
	// Open is SELECT or EXPUNGE: a record that stopped matching goes now.
	Open Mode = iota
	// Poll is a pass in an open mailbox: such a record stays until EXPUNGE or
	// the next open, so a message does not vanish while it is read.
	Poll
)

// SyncResult is what one pass decided.
type SyncResult struct {
	Header mailbox.VirtualHeader
	// Records carry the uid and modseq they had; zero means a new record, or
	// one whose flags changed and so takes a new modseq.
	Records []mailbox.MessageMeta
	// Rebuilt says the set was defined differently and was built from nothing.
	Rebuilt bool
	// Changed is false when no backing folder moved: nothing to write.
	Changed bool
}

// Sync decides membership by rule, a copy per folder being one message; only
// folders whose state moved since the header saw them are read (virtual-sync.c:1413).
func Sync(cfg *Config, was mailbox.VirtualHeader, old []*mailbox.MessageMeta, mode Mode, r Resolver) (SyncResult, error) {
	out := SyncResult{Header: was}
	if was.NeedsRebuild(cfg.SearchArgsCRC32) {
		out = SyncResult{Header: mailbox.VirtualHeader{SearchCRC32: cfg.SearchArgsCRC32}, Rebuilt: true, Changed: true}
		old = nil
	}
	folders, err := r.Folders(cfg)
	if err != nil {
		return SyncResult{}, err
	}
	oldOf := map[uint32][]*mailbox.MessageMeta{}
	for _, m := range old {
		oldOf[m.VirtualBacking] = append(oldOf[m.VirtualBacking], m)
	}
	present := map[uint32]bool{}
	for _, b := range folders {
		seen, known := out.Header.BackingByGUID(b.GUID)
		id := out.Header.AssignBacking(b.GUID, b.Name, b.UIDValidity)
		present[id] = true
		if known && seen.UIDValidity == b.UIDValidity && seen.NextUID == b.NextUID &&
			seen.HighestModSeq == b.HighestModSeq && seen.HighestModSeq != 0 && !out.Rebuilt {
			for _, m := range oldOf[id] {
				out.Records = append(out.Records, *m)
			}
			continue
		}
		out.Changed = true
		prior := oldOf[id]
		if known && seen.UIDValidity != b.UIDValidity {
			prior = nil // a recreated folder: its old uids name other messages
		}
		recs, pending, perr := syncFolder(cfg, b, id, prior, mode, r)
		if perr != nil {
			return SyncResult{}, perr
		}
		out.Records = append(out.Records, recs...)
		stamp := b.HighestModSeq
		if pending {
			stamp = 0 // the next pass reads it again (virtual-sync.c:1276-1278)
		}
		out.Header.StampBacking(id, b.NextUID, stamp)
	}
	for id := range oldOf {
		if !present[id] {
			out.Changed = true // a folder that left the set takes its records along
		}
	}
	return out, nil
}

// syncFolder decides one moved folder's records against those it had.
func syncFolder(cfg *Config, b Backing, id uint32, prior []*mailbox.MessageMeta, mode Mode, r Resolver) ([]mailbox.MessageMeta, bool, error) {
	msgs, err := r.Messages(b)
	if err != nil {
		return nil, false, fmt.Errorf("virtual: %s: %w", b.Name, err)
	}
	b.Messages = msgs
	keep, err := r.Matches(b, ruleFor(cfg, b.Name))
	if err != nil {
		return nil, false, fmt.Errorf("virtual: %s: %w", b.Name, err)
	}
	had := make(map[uint32]*mailbox.MessageMeta, len(prior))
	for _, m := range prior {
		had[m.VirtualRealUID] = m
	}
	var out []mailbox.MessageMeta
	pending := false
	for _, m := range msgs {
		prev := had[m.UID]
		if keep != nil && !keep[m.UID] {
			if prev == nil || mode == Open {
				continue
			}
			pending = true // stopped matching: kept until EXPUNGE or the next open
		}
		rec := *m
		rec.UID, rec.ModSeq = 0, 0
		rec.VirtualBacking = id
		rec.VirtualRealUID = m.UID
		if prev != nil {
			rec.UID = prev.UID
			if sameFlags(prev, m) {
				rec.ModSeq = prev.ModSeq
			}
		}
		out = append(out, rec)
	}
	return out, pending, nil
}

func sameFlags(a, b *mailbox.MessageMeta) bool {
	return sameSet(a.Flags, b.Flags) && sameSet(a.Keywords, b.Keywords)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	in := make(map[string]bool, len(a))
	for _, x := range a {
		in[strings.ToLower(x)] = true
	}
	for _, x := range b {
		if !in[strings.ToLower(x)] {
			return false
		}
	}
	return true
}

// ruleFor is the rule of the line that named this folder. A folder named by
// several lines takes the first, as the file is read top down.
func ruleFor(cfg *Config, folder string) string {
	for _, b := range cfg.Boxes {
		if b.Negative || b.MetadataEntry != "" {
			continue
		}
		if b.Pattern == folder || matchPattern(b.Pattern, folder) {
			return b.Search
		}
	}
	return ""
}

// matchPattern is the IMAP LIST match of the reference: "*" spans the
// hierarchy, "%" stops at the separator.
func matchPattern(pattern, name string) bool {
	return matchFrom(pattern, name, '/')
}

func matchFrom(pattern, name string, sep byte) bool {
	switch {
	case pattern == "":
		return name == ""
	case pattern[0] == '*':
		for i := 0; i <= len(name); i++ {
			if matchFrom(pattern[1:], name[i:], sep) {
				return true
			}
		}
		return false
	case pattern[0] == '%':
		for i := 0; i <= len(name); i++ {
			if i > 0 && name[i-1] == sep {
				break
			}
			if matchFrom(pattern[1:], name[i:], sep) {
				return true
			}
		}
		return false
	case name == "" || pattern[0] != name[0]:
		return false
	default:
		return matchFrom(pattern[1:], name[1:], sep)
	}
}

// Selects reports whether the configuration takes this folder: a positive line
// names it and no negative line does.
func Selects(cfg *Config, folder string) bool {
	taken := false
	for _, b := range cfg.Boxes {
		if b.MetadataEntry != "" {
			continue // an annotation line selects by metadata, not by name
		}
		if !(b.Pattern == folder || matchPattern(b.Pattern, folder)) {
			continue
		}
		if b.Negative {
			return false
		}
		taken = true
	}
	return taken
}

// SameSeparator rewrites a pattern written with the client's separator into
// the one the folder names use here.
func SameSeparator(pattern, sep string) string {
	if sep == "" || sep == "/" {
		return pattern
	}
	return strings.ReplaceAll(pattern, sep, "/")
}
