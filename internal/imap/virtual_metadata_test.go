package imap_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const taggedConfig = "Projects/*\n/shared/comment:keep*\n"

func seedProjects(t *testing.T, box mailbox.UserMailbox, ui mailbox.UserIndex) {
	for _, name := range []string{"Projects/a", "Projects/b"} {
		box.Create(name) //nolint:errcheck
		saveInto(t, box, ui, name, 1, "in "+name, nil)
	}
}

func withMetadata(t *testing.T, md dict.Dict) func(*imapserver.Options) {
	t.Helper()
	if md == nil {
		var err error
		if md, err = dict.Open(dict.Config{Driver: "memory"}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = md.Close() })
	}
	return func(o *imapserver.Options) { o.MetadataDict = md }
}

// Only the folder whose annotation matches the mask is taken, and a changed
// annotation is seen on the next NOOP (virtual-config.c:420-441).
func TestAnAnnotationLineFiltersTheBackingFolders(t *testing.T) {
	conn, rd := virtualServerWith(t, map[string]string{"Tagged": taggedConfig}, seedProjects, nil, withMetadata(t, nil))
	for tag, cmd := range map[string]string{
		"m1": `SETMETADATA Projects/a (/shared/comment "keep this")`,
		"m2": `SETMETADATA Projects/b (/shared/comment "drop")`,
	} {
		if answer := last(tagged(t, conn, rd, tag, cmd)); !strings.HasPrefix(answer, tag+" OK") {
			t.Fatalf("%s answered %q", cmd, answer)
		}
	}
	if got := existsCount(t, conn, rd, "a1", "Virtual/Tagged"); got != 1 {
		t.Fatalf("EXISTS = %d, want Projects/a only", got)
	}
	if s := subjectOf(t, command(t, conn, rd, "a2", "FETCH 1 (BODY.PEEK[HEADER.FIELDS (SUBJECT)])")); s != "in Projects/a" {
		t.Errorf("the virtual mailbox holds %q, want the message in Projects/a", s)
	}
	other, ord := loginTo(t, lastVirtualAddr)
	command(t, other, ord, "b1", `SETMETADATA Projects/b (/shared/comment "keep that too")`)
	if got := linesWith(command(t, conn, rd, "a3", "NOOP"), " EXISTS"); len(got) != 1 || got[0] != "* 2 EXISTS" {
		t.Errorf("NOOP after Projects/b was annotated answered %v, want * 2 EXISTS", got)
	}
}

// flakyDict fails its lookups while down; writes still reach the store.
type flakyDict struct {
	dict.Dict
	down atomic.Bool
}

func (f *flakyDict) Lookup(ctx context.Context, set *dict.OpSettings, key string) ([][]byte, bool, error) {
	if f.down.Load() {
		return nil, false, errors.New("dict is down")
	}
	return f.Dict.Lookup(ctx, set, key)
}

// A dict that cannot be read leaves the set as it was: read as "not set", the
// inverted line would take Projects/b in.
func TestAnAnnotationThatCannotBeReadLeavesTheSet(t *testing.T) {
	inner, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	md := &flakyDict{Dict: inner}
	conn, rd := virtualServerWith(t, map[string]string{"Tagged": "Projects/*\n-/shared/comment:drop*\n"}, seedProjects, nil, withMetadata(t, md))
	command(t, conn, rd, "m1", `SETMETADATA Projects/b (/shared/comment "drop")`)
	if got := existsCount(t, conn, rd, "a1", "Virtual/Tagged"); got != 1 {
		t.Fatalf("EXISTS = %d, want Projects/a only", got)
	}
	md.down.Store(true)
	if got := linesWith(command(t, conn, rd, "a2", "NOOP"), " EXISTS"); len(got) != 0 {
		t.Errorf("NOOP with the dict down answered %v, want the set unchanged", got)
	}
}
