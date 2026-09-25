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
	// UIDValidity and Messages are the folder as it stands now.
	UIDValidity uint32
	Messages    []*mailbox.MessageMeta
}

// Resolver hands the sync the folders a configuration names. Patterns are
// expanded by the caller, which is where the namespace and its separator are.
type Resolver interface {
	// Folders returns every backing folder the configuration selects, after
	// the negative lines have taken theirs out.
	Folders(cfg *Config) ([]Backing, error)
	// Matches says which of the folder's messages the rule keeps. A rule that
	// is empty keeps them all.
	Matches(b Backing, rule string) (map[uint32]bool, error)
}

// SyncResult is what one pass decided.
type SyncResult struct {
	Header  mailbox.VirtualHeader
	Records []mailbox.MessageMeta
	// Rebuilt says the set was defined differently and was built from nothing.
	Rebuilt bool
}

// Sync works out what the mailbox holds: the rule decides membership here,
// not at search time, and a copy in two folders is two messages.
func Sync(cfg *Config, was mailbox.VirtualHeader, r Resolver) (SyncResult, error) {
	out := SyncResult{Header: was}
	if was.NeedsRebuild(cfg.SearchArgsCRC32) {
		out = SyncResult{Header: mailbox.VirtualHeader{SearchCRC32: cfg.SearchArgsCRC32}, Rebuilt: true}
	}
	folders, err := r.Folders(cfg)
	if err != nil {
		return SyncResult{}, err
	}
	for _, b := range folders {
		rule := ruleFor(cfg, b.Name)
		keep, merr := r.Matches(b, rule)
		if merr != nil {
			return SyncResult{}, fmt.Errorf("virtual: %s: %w", b.Name, merr)
		}
		id := out.Header.AssignBacking(b.GUID, b.Name, b.UIDValidity)
		for _, m := range b.Messages {
			if keep != nil && !keep[m.UID] {
				continue
			}
			rec := *m
			rec.UID = 0 // the virtual uid is assigned by the index, not here
			rec.VirtualBacking = id
			rec.VirtualRealUID = m.UID
			out.Records = append(out.Records, rec)
		}
	}
	return out, nil
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
