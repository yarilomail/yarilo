package mailbox

import "strings"

// NamespaceShape is what decides a namespace's role in the set: enough of a
// configured namespace for the config loader and a session to agree on it.
type NamespaceShape struct {
	Type     string
	Location string
	Inbox    bool
}

func isPersonal(t string) bool { return strings.EqualFold(strings.TrimSpace(t), "personal") }

// PrimaryPersonalIndex names the namespace owning INBOX: marked inbox, else the
// first personal one without a location, else the first personal; -1 if none.
func PrimaryPersonalIndex(shapes []NamespaceShape) int {
	first, bare := -1, -1
	for i, s := range shapes {
		if !isPersonal(s.Type) {
			continue
		}
		if s.Inbox {
			return i
		}
		if first < 0 {
			first = i
		}
		if bare < 0 && s.Location == "" {
			bare = i
		}
	}
	if bare >= 0 {
		return bare
	}
	return first
}

// OwnsStore reports a personal namespace with storage of its own: a location,
// and not the INBOX namespace, whose location names only the driver.
func OwnsStore(shapes []NamespaceShape, i int) bool {
	if i < 0 || i >= len(shapes) || !isPersonal(shapes[i].Type) || shapes[i].Location == "" {
		return false
	}
	return i != PrimaryPersonalIndex(shapes)
}

// SubsFileFor is namespace i's subscriptions file: one that owns a store takes
// its own, and every other keeps the name it had, so no deployment's file moves.
func SubsFileFor(shapes []NamespaceShape, i int, prefix, separator string) string {
	if OwnsStore(shapes, i) {
		return "subscriptions-" + NamespaceFileSlug(prefix, separator, "personal")
	}
	return NamespaceSubsFile(prefix, separator, shapes[i].Type)
}
