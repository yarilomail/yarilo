// Package search evaluates IMAP search criteria over a folder's messages, with
// the full-text index where it can answer: SEARCH and virtual mailboxes use it.
package search

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/yarilomail/yarilo/internal/fts/language"
	ftsquery "github.com/yarilomail/yarilo/internal/fts/query"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Options wires the full-text index in; with no Client every search scans.
type Options struct {
	Client ftsproto.Client
	// Chain matches the index service's language set, so a query expands
	// through every language indexing could have picked.
	Chain *language.MultiChain
	// AddMissing, ReadFallback, Timeout, Strict: see the FTS documentation.
	AddMissing   string
	ReadFallback bool
	Timeout      time.Duration
	Strict       bool
	// FirstIndexGrace bounds the wait for a mailbox with nothing indexed yet.
	FirstIndexGrace time.Duration
	// Enabled gates searching only; indexing keeps running without it.
	Enabled bool
}

// Indexed reports whether the full-text index answers searches at all.
func (o Options) Indexed() bool { return o.Client != nil && o.Chain != nil && o.Enabled }

var (
	// ErrStillIndexing: the index is behind the folder and no scan may stand in.
	ErrStillIndexing = errors.New("search: the mailbox is still being indexed")
	// ErrLookup: the index refused the query and no scan may stand in.
	ErrLookup = errors.New("search: the full-text lookup failed")
)

// Plan is what the index answered for one folder: the candidates, those it
// could only say maybe about, and the criteria left for the scan.
type Plan struct {
	Covered map[uint32]bool
	Verify  map[uint32]bool
	Rest    *imaplib.SearchCriteria
	// RestNeedsBody: what is left still reads the message, for sent dates.
	RestNeedsBody bool
	// Scores is the engine's own weight per uid, when it gave one.
	Scores map[uint32]float64
}

// NeedsBody reports whether the criteria read the message's bytes.
func NeedsBody(c *imaplib.SearchCriteria) bool {
	return len(c.Header) > 0 || len(c.Body) > 0 || len(c.Text) > 0 ||
		!c.SentSince.IsZero() || !c.SentBefore.IsZero() || nestedNeedsBody(c.Not, c.Or)
}

func nestedNeedsBody(not []imaplib.SearchCriteria, or [][2]imaplib.SearchCriteria) bool {
	for i := range not {
		if NeedsBody(&not[i]) {
			return true
		}
	}
	for i := range or {
		if NeedsBody(&or[i][0]) || NeedsBody(&or[i][1]) {
			return true
		}
	}
	return false
}

// TextOnTop reports whether the criteria carry text the index can answer: at
// the top level, none of it under NOT or OR, which keep the scan's semantics.
func TextOnTop(c *imaplib.SearchCriteria) bool {
	if len(c.Body) == 0 && len(c.Text) == 0 && len(c.Header) == 0 {
		return false
	}
	return !nestedNeedsBody(c.Not, c.Or)
}

// Query turns the top-level text into the engine query and strips it from the
// criteria. impossible: a text part expanded to nothing, so nothing can match.
func (o Options) Query(c *imaplib.SearchCriteria) (q fts.Query, rest *imaplib.SearchCriteria, restNeedsBody, impossible bool) {
	qc := ftsquery.Criteria{Body: c.Body, Text: c.Text}
	for _, h := range c.Header {
		qc.Header = append(qc.Header, ftsquery.Header{Key: h.Key, Value: h.Value})
	}
	q, impossible = ftsquery.Build(o.Chain, qc)
	stripped := *c
	stripped.Body, stripped.Text, stripped.Header = nil, nil, nil
	return q, &stripped, !stripped.SentSince.IsZero() || !stripped.SentBefore.IsZero(), impossible
}

// Plan asks the index for one folder's text. A nil plan means scan; an error
// means the folder cannot be searched now and no scan may stand in.
func (o Options) Plan(user string, mbox fts.MailboxRef, c *imaplib.SearchCriteria, msgs []*mailbox.MessageMeta) (*Plan, error) {
	if !o.Indexed() || !TextOnTop(c) {
		return nil, nil
	}
	q, rest, restNeedsBody, impossible := o.Query(c)
	if impossible {
		return &Plan{Covered: map[uint32]bool{}, Verify: map[uint32]bool{}, Rest: rest, RestNeedsBody: restNeedsBody}, nil
	}
	if scan, err := o.CatchUp(user, mbox, msgs); err != nil || scan {
		return nil, err
	}
	res, err := o.Client.Lookup(user, mbox, q)
	if err != nil {
		slog.Warn("search: fts lookup failed", "user", user, "folder", mbox.Name, "err", err)
		if o.ReadFallback {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %w", ErrLookup, err)
	}
	p := &Plan{
		Covered: make(map[uint32]bool, len(res.Definite)+len(res.Maybe)),
		Verify:  make(map[uint32]bool, len(res.Maybe)),
		Rest:    rest, RestNeedsBody: restNeedsBody,
	}
	for _, uid := range res.Definite {
		p.Covered[uid] = true
		if o.Strict {
			p.Verify[uid] = true
		}
	}
	for _, uid := range res.Maybe {
		p.Covered[uid] = true
		p.Verify[uid] = true
	}
	if len(res.Scores) > 0 {
		p.Scores = make(map[uint32]float64, len(res.Scores))
		for _, sc := range res.Scores {
			p.Scores[sc.UID] = sc.Value
		}
	}
	// Counts only, never the terms: whether the index had hits at all.
	slog.Debug("search: fts candidates", "user", user, "folder", mbox.Name,
		"definite", len(res.Definite), "maybe", len(res.Maybe))
	return p, nil
}

// CatchUp waits for an index behind the folder, after asking for it first.
// scan: give up and scan; an error: neither, the caller answers retry.
func (o Options) CatchUp(user string, mbox fts.MailboxRef, msgs []*mailbox.MessageMeta) (scan bool, err error) {
	if o.AddMissing == "" || len(msgs) == 0 {
		return false, nil
	}
	maxUID := msgs[len(msgs)-1].UID
	last, _, err := o.Client.Status(user, mbox)
	if err == nil && last >= maxUID {
		return false, nil
	}
	var serviceErr error
	if err != nil {
		serviceErr = err
		slog.Warn("search: fts status failed", "user", user, "err", err)
	} else if perr := o.Client.Prepend(user, mbox, maxUID); perr != nil {
		serviceErr = perr
		slog.Warn("search: fts prepend failed", "user", user, "err", perr)
	} else {
		var caughtUp bool
		caughtUp, serviceErr = o.wait(user, mbox, last, maxUID)
		if caughtUp {
			return false, nil
		}
	}
	if o.ReadFallback {
		return true, nil
	}
	if errors.Is(serviceErr, ftsproto.ErrUnavailable) {
		return false, serviceErr
	}
	return false, ErrStillIndexing
}

// wait polls the checkpoint until it reaches maxUID, stalls, or the timeout
// passes; a cold index is given its grace before a stall counts (#1379).
func (o Options) wait(user string, mbox fts.MailboxRef, last, maxUID uint32) (bool, error) {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	// ~2s of a flat checkpoint is a broken engine, not a slow one.
	const maxStallPolls = 8
	best, stalls, reason := last, 0, "timed out"
	cold := last == 0
	coldGrace := o.FirstIndexGrace
	if coldGrace <= 0 {
		coldGrace = 10 * time.Second
	}
	if coldGrace > timeout {
		coldGrace = timeout
	}
	coldDeadline := time.Now().Add(coldGrace)
	var serviceErr error
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		cur, _, serr := o.Client.Status(user, mbox)
		if serr != nil {
			serviceErr, reason = serr, "status error"
			break
		}
		last = cur
		if cur >= maxUID {
			return true, nil
		}
		if cur > best {
			best, stalls, cold = cur, 0, false
			continue
		}
		if cold {
			if time.Now().After(coldDeadline) {
				reason = "first index did not start within the grace"
				break
			}
			continue
		}
		if stalls++; stalls >= maxStallPolls {
			reason = "no progress"
			break
		}
	}
	if cold && reason == "timed out" {
		reason = "first index not built within the timeout"
	}
	slog.Warn("search: fts catch-up giving up", "user", user, "folder", mbox.Name,
		"reason", reason, "indexed", last, "want", maxUID, "cold", cold)
	return false, serviceErr
}

// Match decides one message; raw is its bytes when the criteria read them.
func Match(seqNum uint32, m *mailbox.MessageMeta, c *imaplib.SearchCriteria, raw []byte) bool {
	flags := make([]imaplib.Flag, 0, len(m.Flags)+len(m.Keywords))
	for _, f := range m.Flags {
		flags = append(flags, imaplib.Flag(f))
	}
	for _, k := range m.Keywords {
		flags = append(flags, imaplib.Flag(k))
	}
	return imapserver.MatchMessage(seqNum, imaplib.UID(m.UID), m.InternalDate, int64(m.RFC822Size()), flags, raw, c)
}

// Folder is the uids of msgs a rule keeps, with the index where it answers and
// the scan elsewhere; an empty rule keeps them all (nil).
func (o Options) Folder(box mailbox.Box, user string, f *mailbox.Folder, msgs []*mailbox.MessageMeta, rule string) (map[uint32]bool, error) {
	if strings.TrimSpace(rule) == "" {
		return nil, nil
	}
	c, err := imapserver.ParseSearchCriteria(rule)
	if err != nil {
		return nil, fmt.Errorf("search: rule %q: %w", rule, err)
	}
	plan := o.folderPlan(user, f, c, msgs)
	keep := make(map[uint32]bool, len(msgs))
	for i, m := range msgs {
		crit, needRaw := c, NeedsBody(c)
		if plan != nil {
			if !plan.Covered[m.UID] {
				continue
			}
			if !plan.Verify[m.UID] {
				crit, needRaw = plan.Rest, plan.RestNeedsBody
			}
		}
		var raw []byte
		if needRaw {
			raw = readRaw(box, f.Name, m)
		}
		if Match(uint32(i+1), m, crit, raw) {
			keep[m.UID] = true
		}
	}
	return keep, nil
}

// folderPlan asks the index for a rule's text over one folder, as one set; a
// folder it cannot answer for now is scanned, a rule having nobody to retry.
func (o Options) folderPlan(user string, f *mailbox.Folder, c *imaplib.SearchCriteria, msgs []*mailbox.MessageMeta) *Plan {
	if !o.Indexed() || !TextOnTop(c) {
		return nil
	}
	q, rest, restNeedsBody, impossible := o.Query(c)
	if impossible {
		return &Plan{Covered: map[uint32]bool{}, Verify: map[uint32]bool{}, Rest: rest, RestNeedsBody: restNeedsBody}
	}
	mbox := RefOf(f)
	if scan, err := o.CatchUp(user, mbox, msgs); err != nil || scan {
		return nil
	}
	res, err := o.Client.LookupIn(user, []fts.MailboxRef{mbox}, q)
	if err != nil {
		slog.Warn("search: rule lookup failed", "user", user, "folder", f.Name, "err", err)
		return nil
	}
	p := &Plan{Covered: map[uint32]bool{}, Verify: map[uint32]bool{}, Rest: rest, RestNeedsBody: restNeedsBody}
	for _, h := range res.Definite {
		if h.Folder == mbox.GUID {
			p.Covered[h.UID] = true
		}
	}
	for _, h := range res.Maybe {
		if h.Folder == mbox.GUID {
			p.Covered[h.UID] = true
			p.Verify[h.UID] = true
		}
	}
	return p
}

// RefOf is a folder's identity on the wire to the index service.
func RefOf(f *mailbox.Folder) fts.MailboxRef {
	return fts.MailboxRef{Name: f.Name, GUID: mailbox.FormatObjectID(f.GUID), UIDValidity: f.UIDValidity}
}

func readRaw(box mailbox.Box, folder string, m *mailbox.MessageMeta) []byte {
	rc, err := box.OpenMessage(folder, m)
	if err != nil {
		return nil
	}
	defer rc.Close() //nolint:errcheck
	raw, _ := io.ReadAll(rc)
	return raw
}
