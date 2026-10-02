package managesieve

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A script name that walks out of the home is refused by every command, and
// nothing outside the home is touched.
func TestAScriptNameCannotLeaveTheHome(t *testing.T) {
	root := t.TempDir()
	home, other := filepath.Join(root, "alice"), filepath.Join(root, "bob")
	for _, d := range []string{home, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c := runSession(t, newTestStore(), home)
	c.conn.SetReadDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	src := "keep;\r\n"
	c.send("PUTSCRIPT \"plain\" {" + itoa(len(src)) + "+}\r\n" + src + "\r\n")
	if lines, ok := c.readUntilResult(); !ok {
		t.Fatalf("PUTSCRIPT plain = %q, want OK", lines)
	}
	bad := "/../bob/.yarilo"
	for _, cmd := range []string{
		"PUTSCRIPT \"" + bad + "\" {" + itoa(len(src)) + "+}\r\n" + src + "\r\n",
		"GETSCRIPT \"" + bad + "\"\r\n",
		"DELETESCRIPT \"" + bad + "\"\r\n",
		"SETACTIVE \"" + bad + "\"\r\n",
		"RENAMESCRIPT \"plain\" \"" + bad + "\"\r\n",
		"RENAMESCRIPT \"" + bad + "\" \"other\"\r\n",
		"PUTSCRIPT \"a/b\" {" + itoa(len(src)) + "+}\r\n" + src + "\r\n",
	} {
		c.send(cmd)
		lines, _ := c.readUntilResult()
		if got := lines[len(lines)-1]; got != `NO "Invalid script name."` {
			t.Errorf("%q = %q, want NO \"Invalid script name.\"", strings.SplitN(cmd, "\r\n", 2)[0], got)
		}
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatalf("bob's home gained %d entries", len(entries))
	}
	if entries, _ := os.ReadDir(root); len(entries) != 2 {
		t.Fatalf("the mail root has %d entries, want 2", len(entries))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
