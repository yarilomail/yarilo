// Package virtual holds the virtual mailbox: a mailbox whose messages live in
// other folders. This file is its configuration, in the reference's syntax.
package virtual

import (
	"bufio"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// ConfigFileName is ours; LegacyConfigFileName is read when ours is not
	// there, the way the uid list is adopted (#1841).
	ConfigFileName       = "yarilo-virtual"
	LegacyConfigFileName = "dovecot-virtual"
)

// Box is one line of the configuration: a mailbox pattern with what the
// prefix said about it, and the search rule that narrows what it contributes.
type Box struct {
	// Pattern is the mailbox name or wildcard pattern, without its prefix.
	Pattern string
	// ClearRecent is "+": the folder is opened so that \Recent is cleared.
	ClearRecent bool
	// Negative is "-": what it matches is taken out of the set.
	Negative bool
	// Save is "!": SAVE and COPY into the virtual mailbox land here. One per
	// configuration, and never a pattern.
	Save bool
	// MetadataEntry/MetadataValue select folders by an annotation instead of
	// by name ("/entry:value"); such a line carries no wildcards either.
	MetadataEntry string
	MetadataValue string
	// Search is the IMAP SEARCH text for this box, kept as text: the parser
	// SEARCH uses is unexported upstream, so a bad rule fails on first run.
	Search string
}

// HasWildcard reports whether the pattern matches more than one folder.
func (b Box) HasWildcard() bool {
	return strings.ContainsAny(b.Pattern, "*%")
}

// Config is a virtual mailbox's whole configuration.
type Config struct {
	Boxes []Box
	// SaveTo is the box SAVE and COPY go to, nil when none is named: the
	// mailbox then refuses them rather than choosing one.
	SaveTo *Box
	// SearchArgsCRC32 covers every rule in the file, in order. A change means
	// the set is not what it was, so the index is rebuilt rather than extended.
	SearchArgsCRC32 uint32
}

// ErrNoConfig says the mailbox has no configuration file.
var ErrNoConfig = errors.New("virtual: no configuration")

// LoadConfig reads the configuration from the mailbox directory: ours first,
// then the foreign name, which is read but never written.
func LoadConfig(dir string) (*Config, error) {
	for _, name := range []string{ConfigFileName, LegacyConfigFileName} {
		f, err := os.Open(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("virtual: open %s: %w", name, err)
		}
		defer f.Close() //nolint:errcheck
		return ParseConfig(f)
	}
	return nil, ErrNoConfig
}

// ParseConfig reads the configuration: one mailbox per line, and an indented
// line continues the search rule of the mailboxes named since the last one.
func ParseConfig(r io.Reader) (*Config, error) {
	cfg := &Config{}
	sc := bufio.NewScanner(r)
	rule := ""
	ruleFrom := 0 // boxes from this index on take the rule being read
	flush := func() error {
		if strings.TrimSpace(rule) == "" {
			rule = ""
			return nil
		}
		if ruleFrom == len(cfg.Boxes) {
			return errors.New("virtual: search rule without a mailbox")
		}
		text := strings.TrimSpace(rule)
		cfg.SearchArgsCRC32 = crc32.Update(cfg.SearchArgsCRC32, crc32.IEEETable, []byte(text))
		for i := ruleFrom; i < len(cfg.Boxes); i++ {
			cfg.Boxes[i].Search = text
		}
		rule, ruleFrom = "", len(cfg.Boxes)
		return nil
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if ruleFrom == len(cfg.Boxes) {
				return nil, errors.New("virtual: search rule without a mailbox")
			}
			rule += " " + strings.TrimSpace(line)
			continue
		}
		if err := flush(); err != nil {
			return nil, err
		}
		box, err := parseBoxLine(line)
		if err != nil {
			return nil, err
		}
		if box.Save {
			if cfg.SaveTo != nil {
				return nil, errors.New("virtual: more than one save mailbox")
			}
		}
		cfg.Boxes = append(cfg.Boxes, box)
		if box.Save {
			cfg.SaveTo = &cfg.Boxes[len(cfg.Boxes)-1]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("virtual: read configuration: %w", err)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(cfg.Boxes) == 0 {
		return nil, errors.New("virtual: the configuration names no mailbox")
	}
	return cfg, nil
}

func parseBoxLine(line string) (Box, error) {
	var b Box
	if !utf8.ValidString(line) {
		return b, fmt.Errorf("virtual: mailbox name is not UTF-8: %q", line)
	}
	noWildcards := false
	switch line[0] {
	case '+':
		b.ClearRecent, line = true, line[1:]
	case '-':
		b.Negative, line = true, line[1:]
	case '!':
		b.Save, line, noWildcards = true, line[1:], true
	}
	if strings.HasPrefix(line, "/") {
		entry, value, ok := strings.Cut(line[1:], ":")
		if !ok {
			return b, errors.New("virtual: ':' missing between the annotation and its value")
		}
		if entry == "" {
			return b, errors.New("virtual: the annotation has no name")
		}
		b.MetadataEntry, b.MetadataValue = "/"+entry, value
		noWildcards = true
	}
	b.Pattern = line
	if strings.EqualFold(b.Pattern, "INBOX") {
		b.Pattern = "INBOX"
	}
	if b.Pattern == "" && b.MetadataEntry == "" {
		return b, errors.New("virtual: the line names no mailbox")
	}
	if noWildcards && b.HasWildcard() {
		what := "a save mailbox"
		if b.MetadataEntry != "" {
			what = "an annotation line"
		}
		return b, fmt.Errorf("virtual: %s carries no wildcard: %q", what, b.Pattern)
	}
	return b, nil
}
