package managesieve

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Two scripts stored, a count limit of two and a size limit of 100 bytes.
func TestScriptQuota(t *testing.T) {
	cases := []struct {
		name string
		send string
		want string // prefix of the tagged reply
	}{
		{"PUTSCRIPT of a new name past the count", "PUTSCRIPT \"c\" {6+}\r\nkeep;\n",
			`NO (QUOTA/MAXSCRIPTS) "Script count quota exceeded (max 2 scripts)."`},
		{"PUTSCRIPT replacing a stored name at the count", "PUTSCRIPT \"b\" {6+}\r\nkeep;\n", "OK"},
		{"HAVESPACE within quota", "HAVESPACE \"b\" 100\r\n", `OK "Putscript would succeed."`},
		{"HAVESPACE of a new name past the count", "HAVESPACE \"c\" 10\r\n",
			`NO (QUOTA/MAXSCRIPTS) "Script count quota exceeded (max 2 scripts)."`},
		{"HAVESPACE past the size", "HAVESPACE \"b\" 101\r\n",
			`NO (QUOTA/MAXSIZE) "Script is too large (max 100 bytes)."`},
		{"HAVESPACE with a literal name", "HAVESPACE {1+}\r\nb 5\r\n", "OK"},
		{"HAVESPACE with words after the size", "HAVESPACE \"b\" 5 junk\r\n", "OK"},
		{"HAVESPACE of an empty script", "HAVESPACE \"b\" 0\r\n", `NO "Cannot upload empty script."`},
		{"HAVESPACE without a size", "HAVESPACE \"b\"\r\n", `NO "Invalid scriptsize argument."`},
		{"HAVESPACE with a size that is not a number", "HAVESPACE \"b\" ten\r\n", `NO "Invalid scriptsize argument."`},
		{"HAVESPACE of the reserved name", "HAVESPACE \"yarilo\" 5\r\n", `NO "Script name is reserved."`},
		{"HAVESPACE of an invalid name", "HAVESPACE \"\" 5\r\n", `NO "Invalid script name."`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, home := newTestStore(), t.TempDir()
			for _, n := range []string{"a", "b"} {
				if err := store.SaveScript(ctx, "u1@example.com", home, n, []byte("keep;\n")); err != nil {
					t.Fatal(err)
				}
			}
			c := runSessionLimits(t, store, home, 100, 2, 0)
			c.conn.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
			c.send(tc.send)
			lines, _ := c.readUntilResult()
			if got := lines[len(lines)-1]; !strings.HasPrefix(got, tc.want) {
				t.Fatalf("reply = %q, want %q", got, tc.want)
			}
			// The whole command line was consumed: the next command is read as one.
			c.send("NOOP\r\n")
			if lines, ok := c.readUntilResult(); !ok {
				t.Fatalf("NOOP after the reply = %q, want OK", lines)
			}
			names, err := store.ListScripts(ctx, "u1@example.com", home)
			if err != nil || len(names) != 2 {
				t.Errorf("scripts after the command = %v (err %v), want the two stored", names, err)
			}
		})
	}
}

// sieve_max_num_scripts 0 lifts the count limit.
func TestAZeroScriptCountLiftsTheLimit(t *testing.T) {
	ctx := context.Background()
	store, home := newTestStore(), t.TempDir()
	c := runSessionLimits(t, store, home, 100, 0, 0)
	for _, n := range []string{"a", "b", "c"} {
		c.send("PUTSCRIPT \"" + n + "\" {6+}\r\nkeep;\n")
		if lines, ok := c.readUntilResult(); !ok {
			t.Fatalf("PUTSCRIPT %s = %q, want OK", n, lines)
		}
	}
	if names, _ := store.ListScripts(ctx, "u1@example.com", home); len(names) != 3 {
		t.Errorf("scripts = %v, want three", names)
	}
}

// Two 6-byte scripts stored and a storage limit of 20 bytes.
func TestScriptStorageQuota(t *testing.T) {
	cases := []struct {
		name, send, want string
	}{
		{"PUTSCRIPT of a new name past the storage", "PUTSCRIPT \"c\" {9+}\r\ndiscard;\n",
			`NO (QUOTA/MAXSTORAGE) "Script storage quota exceeded (max 20 bytes)."`},
		{"PUTSCRIPT filling the storage exactly", "PUTSCRIPT \"c\" {8+}\r\nstop;\n\n\n", "OK"},
		{"HAVESPACE past the storage", "HAVESPACE \"c\" 9\r\n",
			`NO (QUOTA/MAXSTORAGE) "Script storage quota exceeded (max 20 bytes)."`},
		{"HAVESPACE replacing a stored script", "HAVESPACE \"b\" 14\r\n", `OK "Putscript would succeed."`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, home := newTestStore(), t.TempDir()
			for _, n := range []string{"a", "b"} {
				if err := store.SaveScript(ctx, "u1@example.com", home, n, []byte("keep;\n")); err != nil {
					t.Fatal(err)
				}
			}
			c := runSessionLimits(t, store, home, 100, 0, 20)
			c.conn.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
			c.send(tc.send)
			lines, _ := c.readUntilResult()
			if got := lines[len(lines)-1]; !strings.HasPrefix(got, tc.want) {
				t.Fatalf("reply = %q, want %q", got, tc.want)
			}
		})
	}
}

// RENAMESCRIPT onto a stored name is refused by the store, under its lock, and
// both scripts stay (RFC 5804 §2.11.1).
func TestRenameOntoAStoredNameIsRefused(t *testing.T) {
	ctx := context.Background()
	store, home := newTestStore(), t.TempDir()
	_ = store.SaveScript(ctx, "u1@example.com", home, "a", []byte("keep;\n"))
	_ = store.SaveScript(ctx, "u1@example.com", home, "b", []byte("discard;\n"))
	c := runSession(t, store, home)
	c.conn.SetDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	c.send("RENAMESCRIPT \"a\" \"b\"\r\n")
	lines, _ := c.readUntilResult()
	if got := lines[len(lines)-1]; !strings.HasPrefix(got, "NO (ALREADYEXISTS)") {
		t.Fatalf("reply = %q, want NO (ALREADYEXISTS)", got)
	}
	if src, _, _ := store.GetScript(ctx, "u1@example.com", home, "b"); string(src) != "discard;\n" {
		t.Errorf("b holds %q after the refused rename", src)
	}
}
