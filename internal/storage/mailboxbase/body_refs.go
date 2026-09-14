package mailboxbase

import "github.com/yarilomail/yarilo/pkg/mailbox"

// bodyRefs counts how many records name each file: a damaged mailbox holds
// several for one file, and the first expunge must not strip the others.
type bodyRefs map[string]int

// newBodyRefs counts how many records point at each body: two records naming
// one file must not both unlink it.
func newBodyRefs(names []string) bodyRefs {
	r := make(bodyRefs, len(names))
	for _, n := range names {
		if n != "" {
			r[n]++
		}
	}
	return r
}

// bodyNames asks the driver what each record is called, which is the only place
// a name comes from now (#1700).
func bodyNames(box mailbox.UserMailbox, folder string, msgs []*mailbox.MessageMeta) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if name, err := MessagePath(box, folder, m); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// bodyFate is what an expunge does with the record's body.
type bodyFate int

const (
	// bodyNameless: the record carries no filename, so there is nothing to free
	// and nothing referring to anything (#1693).
	bodyNameless bodyFate = iota
	// bodyShared: another record still names this file.
	bodyShared
	// bodyFree: this was the last record naming it.
	bodyFree
)

// fate drops one reference and says what to do with the body. The three cases
// were two before, and an empty name read as "another record points at it".
func (r bodyRefs) fate(filename string) bodyFate {
	if filename == "" {
		return bodyNameless
	}
	r[filename]--
	if r[filename] <= 0 {
		return bodyFree
	}
	return bodyShared
}
