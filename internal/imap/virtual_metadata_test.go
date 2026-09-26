package imap_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

const invertedConfig = "Projects/*\n-/shared/comment:drop*\n"

// downServer opens Virtual/Tagged with Projects/a only, then takes the dict
// down: read as "not set", the inverted line would take Projects/b in.
func downServer(t *testing.T, tune func(*imapserver.Options)) (net.Conn, *bufio.Reader, *flakyDict) {
	t.Helper()
	inner, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	md := &flakyDict{Dict: inner}
	conn, rd := virtualServerWith(t, map[string]string{"Tagged": invertedConfig}, seedProjects, nil, func(o *imapserver.Options) {
		withMetadata(t, md)(o)
		if tune != nil {
			tune(o)
		}
	})
	command(t, conn, rd, "m1", `SETMETADATA Projects/b (/shared/comment "drop")`)
	if got := existsCount(t, conn, rd, "m2", "Virtual/Tagged"); got != 1 {
		t.Fatalf("EXISTS = %d, want Projects/a only", got)
	}
	md.down.Store(true)
	return conn, rd, md
}

// A pass that fails in a poll is reported untagged and the command completes,
// the set as the last pass left it (imap-sync.c:637-639).
func TestAFailedPassInAPollIsReportedUntagged(t *testing.T) {
	conn, rd, md := downServer(t, nil)
	lines := tagged(t, conn, rd, "a1", "NOOP")
	if answer := last(lines); !strings.HasPrefix(answer, "a1 OK") {
		t.Errorf("NOOP answered %q, want OK", answer)
	}
	if got := linesWith(lines, "* NO [SERVERBUG]"); len(got) != 1 {
		t.Errorf("NOOP reported %v, want one untagged NO [SERVERBUG]", lines)
	}
	if got := linesWith(lines, " EXISTS"); len(got) != 0 {
		t.Errorf("NOOP with the dict down answered %v, want the set unchanged", got)
	}
	md.down.Store(false)
	if lines := tagged(t, conn, rd, "a2", "NOOP"); len(linesWith(lines, "* NO")) != 0 {
		t.Errorf("NOOP with the dict back answered %v, want no failure", lines)
	}
}

// Opening or counting a mailbox whose pass fails is refused, as a storage
// failure is: its records would describe a set the configuration does not.
func TestAFailedPassRefusesSelectAndStatus(t *testing.T) {
	for _, cmd := range []string{"SELECT Virtual/Tagged", "EXAMINE Virtual/Tagged", "STATUS Virtual/Tagged (MESSAGES)"} {
		t.Run(cmd, func(t *testing.T) {
			conn, rd, _ := downServer(t, nil)
			command(t, conn, rd, "u1", "UNSELECT")
			if answer := last(tagged(t, conn, rd, "a1", cmd)); !strings.HasPrefix(answer, "a1 NO [SERVERBUG]") {
				t.Errorf("%s answered %q, want NO [SERVERBUG]", cmd, answer)
			}
		})
	}
}

// IDLE reports a failed pass when it starts and when a folder it draws from
// wakes it, and stays open.
func TestAFailedPassInIdleIsReportedUntagged(t *testing.T) {
	conn, rd, _ := downServer(t, virtualLocks(t))
	other, ord := loginTo(t, lastVirtualAddr)
	fmt.Fprintf(conn, "a1 IDLE\r\n")
	waitFor(t, conn, rd, "+ ", 2*time.Second)
	if line := waitFor(t, conn, rd, "* NO", 3*time.Second); !strings.HasPrefix(line, "* NO [SERVERBUG]") {
		t.Fatalf("IDLE started with %q, want * NO [SERVERBUG]", line)
	}
	appendRaw(t, other, ord, "b1", "Projects/a", "second")
	if line := waitFor(t, conn, rd, "* NO", 3*time.Second); !strings.HasPrefix(line, "* NO [SERVERBUG]") {
		t.Errorf("IDLE woken answered %q, want * NO [SERVERBUG]", line)
	}
	fmt.Fprintf(conn, "DONE\r\n")
	if line := waitFor(t, conn, rd, "a1 ", 3*time.Second); !strings.Contains(line, "OK") {
		t.Errorf("IDLE ended %q, want OK", line)
	}
}
