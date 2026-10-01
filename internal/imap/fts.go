package imap

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/fts/language"
	"github.com/yarilomail/yarilo/internal/storage/search"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// FTSOptions wires full-text search into IMAP sessions. Nil Client disables
// FTS entirely — SEARCH keeps the sequential scan.
type FTSOptions struct {
	Client ftsproto.Client
	// Chain must match the yarilo-fts service's configured language SET
	// (order-independent) so query expansion covers exactly the languages
	// indexing could have picked. Query expansion is deliberately asymmetric
	// from indexing: it fans out through every configured language, OR'd
	// together, since a query doesn't know which single language a given
	// message was auto-detected as.
	Chain *language.MultiChain
	// AddMissing / ReadFallback / Timeout / Strict — see https://doc.yarilomail.org/FTS §11.
	AddMissing   string
	ReadFallback bool
	Timeout      time.Duration
	Strict       bool
	// FirstIndexGrace bounds the wait for a mailbox with nothing indexed yet.
	// See the cold-start comment in ftsCatchUp: the lag heuristic needs
	// movement to judge, and a first index provides none (#1379).
	FirstIndexGrace time.Duration
	// Autoindex triggers INDEX on delivered events; MaxRecent is the
	// autoindex throttle forwarded to the service.
	Autoindex bool
	MaxRecent int
	// SearchEnabled gates SEARCH only (fts_search): false degrades every SEARCH
	// to the sequential scan while indexing/autoindex/write-through keep
	// running. An incident-response knob for "the FTS engine is misbehaving,
	// stop querying it, but don't let the index go stale." Distinct from
	// fts.enabled (all-or-nothing, including indexing) at the config layer.
	SearchEnabled bool
}

// folderGUIDWarned dedupes the missing-GUID warning to one per folder per
// process; the condition is a property of the folder, not of the message.
var folderGUIDWarned sync.Map

func warnFolderWithoutGUID(user, folder string) {
	if _, seen := folderGUIDWarned.LoadOrStore(user+"\x00"+folder, struct{}{}); seen {
		return
	}
	slog.Warn("imap: folder has no GUID; full-text indexing skipped for it",
		"user", user, "folder", folder)
}

// searchOptions is the evaluator's share of the FTS wiring.
func (o FTSOptions) searchOptions() search.Options {
	return search.Options{
		Client: o.Client, Chain: o.Chain, AddMissing: o.AddMissing, ReadFallback: o.ReadFallback,
		Timeout: o.Timeout, Strict: o.Strict, FirstIndexGrace: o.FirstIndexGrace, Enabled: o.SearchEnabled,
	}
}

// searchError answers a search the index could not serve: retry when the
// service waits on a dependency or is still indexing, NO otherwise (#1409).
func searchError(err error) *imaplib.Error {
	switch {
	case errors.Is(err, ftsproto.ErrUnavailable):
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCodeUnavailable,
			Text: "Full-text search is temporarily unavailable, try again"}
	case errors.Is(err, search.ErrStillIndexing):
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Mailbox is still being indexed, try again later"}
	}
	return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Full-text search unavailable"}
}

// ftsNotify fires the delivery/expunge hooks toward the yarilo-fts service —
// best-effort and asynchronous: the index heals via rescan if a hook is lost.
// Only the folder name travels; the service resolves the rest itself.
func (s *session) ftsNotify(f *mailbox.Folder, expunged bool, uid uint32, guid [16]byte) {
	o := s.srv.opts.FTS
	if o.Client == nil || s.userInfo == nil || f == nil || f.Name == "" {
		return
	}
	if !expunged && !o.Autoindex {
		return
	}
	user := s.userInfo.Username
	// The GUID is the folder's identity for the index, and it is what the
	// index path is keyed by -- an empty one would name a path built from a
	// value that is not there, so the hook is skipped instead (#1183). Said
	// once per folder: per message it would drown the reason it matters.
	mbox := search.RefOf(f)
	if mbox.GUID == "" {
		warnFolderWithoutGUID(user, f.Name)
		return
	}
	go func() {
		var err error
		if expunged {
			err = o.Client.Expunge(user, mbox, uid, guid)
		} else {
			err = o.Client.Index(user, mbox, uid, o.MaxRecent)
		}
		if err != nil {
			slog.Debug("imap: fts notify failed",
				"user", user, "folder", mbox.Name, "expunged", expunged, "err", err)
			return
		}
		// Breadcrumb: confirm the FTS index/expunge notify was sent, so an
		// indexing gap (message delivered but never handed to FTS) is visible.
		slog.Debug("imap: fts notify sent",
			"user", user, "folder", mbox.Name, "uid", uid, "expunged", expunged)
	}()
}

// ftsDropFolder retracts a deleted mailbox, best-effort off the command path:
// a rescan's orphan sweep is what makes a lost one harmless (#2022).
func (s *session) ftsDropFolder(f *mailbox.Folder) {
	o := s.srv.opts.FTS
	if o.Client == nil || s.userInfo == nil || f == nil || f.Name == "" {
		return
	}
	mbox := search.RefOf(f)
	if mbox.GUID == "" {
		warnFolderWithoutGUID(s.userInfo.Username, f.Name)
		return
	}
	user := s.userInfo.Username
	go func() {
		if err := o.Client.DropFolder(user, mbox); err != nil {
			slog.Warn("imap: fts drop folder failed",
				"user", user, "folder", mbox.Name, "err", err)
		}
	}()
}

// relevancyScores normalizes raw per-UID engine weights to the RFC 4731/6203
// wire range, in order's enumeration order (one score per matched message,
// same order as the ESEARCH ALL data item). A per-result-set linear min-max
// scale to integers 1-100 — never 0, floored at 1 — with diff defaulting to
// 1.0 when every score is equal (avoids divide-by-zero; a uniform set maps to
// the floor value 1 for every message).
//
// order can include UIDs the engine returned no score for (matched only via a
// stripped, non-FTS criterion ANDed onto the search) — a plain map lookup
// would default those to 0.0 and corrupt the set's min-max range, dragging lo
// down to a fabricated zero and compressing everything else. Score-less UIDs
// are excluded from the lo/hi computation and floored to 1: "no ranking
// signal" is not "ranked lowest by the engine."
func relevancyScores(raw map[uint32]float64, order []uint32) []uint32 {
	if len(order) == 0 {
		return nil
	}
	var lo, hi float64
	haveRange := false
	for _, uid := range order {
		v, ok := raw[uid]
		if !ok {
			continue
		}
		if !haveRange || v < lo {
			lo = v
		}
		if !haveRange || v > hi {
			hi = v
		}
		haveRange = true
	}
	diff := hi - lo
	if diff == 0 {
		diff = 1.0
	}
	out := make([]uint32, len(order))
	for i, uid := range order {
		v, ok := raw[uid]
		if !ok || !haveRange {
			out[i] = 1
			continue
		}
		score := (v - lo) / diff * 100
		if score < 1 {
			out[i] = 1
		} else {
			out[i] = uint32(score)
		}
	}
	return out
}
