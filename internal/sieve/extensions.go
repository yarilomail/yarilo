package sieve

import (
	"fmt"
	"slices"

	gosieve "github.com/foxcpp/go-sieve"

	"github.com/yarilomail/yarilo/pkg/config"
)

// SupportedExtensions lists all Sieve extensions available in this build.
var SupportedExtensions = []string{
	"fileinto", "reject", "ereject", "envelope", "encoded-character",
	"variables", "relational", "copy", "subaddress", "environment",
	"body", "vacation", "vacation-seconds", "regex", "date", "index",
	"editheader", "mailbox", "duplicate", "ihave", "special-use", "mailboxid",
	"imap4flags", "fcc", "include", "enotify",
	"spamtest", "spamtestplus", "virustest",
	"foreverypart", "mime", "extracttext", "replace", "enclose",
	"mboxmetadata", "servermetadata", "imapsieve",
	"vnd.yarilo.debug",
	"vnd.yarilo.environment",
	"vnd.yarilo.pipe",
	"vnd.yarilo.filter",
	"vnd.yarilo.report",
	"vnd.yarilo.execute",
}

// ExtensionSet is what one kind of script may use: List (empty: every
// supported extension) less Deny.
type ExtensionSet struct {
	List []string
	Deny []string
}

// UserExtensions is a user's set: sieve_extensions less the global-only ones.
func UserExtensions(c config.SieveConfig) ExtensionSet {
	return ExtensionSet{List: c.SieveExtensions, Deny: c.GlobalExtensions}
}

// AdminExtensions is the set of the administrator's scripts: sieve_extensions
// and sieve_global_extensions (#2290).
func AdminExtensions(c config.SieveConfig) ExtensionSet {
	if len(c.SieveExtensions) == 0 {
		return ExtensionSet{}
	}
	return ExtensionSet{List: append(slices.Clone(c.SieveExtensions), c.GlobalExtensions...)}
}

// Has says whether the set lets a script use ext.
func (x ExtensionSet) Has(ext string) bool {
	return (len(x.List) == 0 || slices.Contains(x.List, ext)) && !slices.Contains(x.Deny, ext)
}

// EffectiveExtensions is the part of SupportedExtensions the set allows: what
// a capability list advertises.
func EffectiveExtensions(x ExtensionSet) []string {
	out := make([]string, 0, len(SupportedExtensions))
	for _, e := range SupportedExtensions {
		if x.Has(e) {
			out = append(out, e)
		}
	}
	return out
}

// CheckExtensions verifies that every extension script uses is in the set and
// names the first one that is not.
func CheckExtensions(script *gosieve.Script, x ExtensionSet) error {
	for _, ext := range script.Extensions() {
		if !x.Has(ext) {
			return fmt.Errorf("extension '%s' is not permitted", ext)
		}
	}
	return nil
}

// CompileOptions are the load options for a script held to the set: ihave
// answers from the same set CheckExtensions holds it to (#2297).
func CompileOptions(x ExtensionSet) gosieve.Options {
	opts := gosieve.DefaultOptions()
	if len(x.List) > 0 || len(x.Deny) > 0 {
		opts.Interp.Allowed = x.Has
	}
	return opts
}
