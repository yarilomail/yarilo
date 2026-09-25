package virtual

import (
	"strings"
	"testing"
)

// The three prefixes mean three different things, and the reference's own
// parser is where they are read from (virtual-config.c:154-170).
func TestPrefixesAreNotInterchangeable(t *testing.T) {
	cfg, err := ParseConfig(strings.NewReader("+INBOX\n-Trash\n!Drafts\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Boxes) != 3 {
		t.Fatalf("parsed %d lines, want 3", len(cfg.Boxes))
	}
	inbox, trash, drafts := cfg.Boxes[0], cfg.Boxes[1], cfg.Boxes[2]
	if !inbox.ClearRecent || inbox.Negative || inbox.Save {
		t.Errorf(`"+INBOX" read as %+v, want the one that clears \Recent`, inbox)
	}
	if !trash.Negative || trash.ClearRecent || trash.Save {
		t.Errorf(`"-Trash" read as %+v, want the negative match`, trash)
	}
	if !drafts.Save || drafts.ClearRecent || drafts.Negative {
		t.Errorf(`"!Drafts" read as %+v, want the save mailbox`, drafts)
	}
	if cfg.SaveTo == nil || cfg.SaveTo.Pattern != "Drafts" {
		t.Errorf("SAVE and COPY land in %v, want Drafts", cfg.SaveTo)
	}
}

// A configuration that names no save mailbox refuses SAVE rather than picking
// one: the reference has no default either.
func TestNoSaveMailboxIsNoSaveMailbox(t *testing.T) {
	cfg, err := ParseConfig(strings.NewReader("INBOX\nArchive/*\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SaveTo != nil {
		t.Errorf("a configuration with no %q line named %v as the save mailbox", "!", cfg.SaveTo)
	}
}

func TestConfigRefusals(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"two save mailboxes", "!Drafts\n!Sent\n"},
		{"a save mailbox with a wildcard", "!Archive/*\n"},
		{"an annotation line with a wildcard", "/private/comment:x*\n"},
		{"an annotation without a value", "/private/comment\n"},
		{"a rule before any mailbox", "  unseen\nINBOX\n"},
		{"an empty configuration", "# nothing here\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseConfig(strings.NewReader(tc.text)); err == nil {
				t.Errorf("%q was accepted", tc.text)
			}
		})
	}
}

// An indented line continues the rule of every mailbox named since the last
// one, and the rules are what the crc covers.
func TestRulesAttachToTheMailboxesAboveThem(t *testing.T) {
	const text = "INBOX\nArchive/*\n  unseen\n  smaller 4096\nSent\n"
	cfg, err := ParseConfig(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Boxes[0].Search; got != "unseen smaller 4096" {
		t.Errorf("INBOX carries rule %q", got)
	}
	if got := cfg.Boxes[1].Search; got != "unseen smaller 4096" {
		t.Errorf("Archive/* carries rule %q", got)
	}
	if got := cfg.Boxes[2].Search; got != "" {
		t.Errorf("Sent carries rule %q, want none: the rule above it was already applied", got)
	}

	other, err := ParseConfig(strings.NewReader("INBOX\nArchive/*\n  seen\n  smaller 4096\nSent\n"))
	if err != nil {
		t.Fatal(err)
	}
	if other.SearchArgsCRC32 == cfg.SearchArgsCRC32 {
		t.Error("two different rules hash to one value, so a changed set would not be noticed")
	}
}
