package backendapi

import (
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"

	ftsquery "github.com/yarilomail/yarilo/internal/fts/query"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ftsExpunge invalidates FTS documents for UIDs a rebuild dropped.
// Best-effort: a lost notify heals on the next fts rescan. No-op
// without an FTS client. One RPC per UID (no batch Expunge yet).
func (s *Server) ftsExpunge(uc *userContext, folder string, copies []mailbox.ExpungedCopy) {
	if s.opts.FTSClient == nil || len(copies) == 0 {
		return
	}
	mbox, err := s.ftsMailboxRef(uc, folder)
	if err != nil {
		slog.Warn("backendapi: fts expunge resolve failed", "user", uc.info.Username, "folder", folder, "err", err)
		return
	}
	for _, c := range copies {
		if err := s.opts.FTSClient.Expunge(uc.info.Username, mbox, c.UID, c.GUID); err != nil {
			slog.Warn("backendapi: fts expunge failed", "user", uc.info.Username, "folder", folder, "uid", c.UID, "err", err)
		}
	}
}

// registerFTSRoutes registers full-text search operator routes.
// GUID/UIDVALIDITY are resolved from the index so callers name
// only user + folder. 501 when no FTS client is set.
func (s *Server) registerFTSRoutes() {
	s.mux.Handle("GET /api/backend/fts/status", s.middleware(s.handleFTSStatus))
	s.mux.Handle("POST /api/backend/fts/rescan", s.middleware(s.handleFTSRescan))
	s.mux.Handle("POST /api/backend/fts/optimize", s.middleware(s.handleFTSOptimize))
	s.mux.Handle("GET /api/backend/fts/lookup", s.middleware(s.handleFTSLookup))
}

// ftsMailboxRef resolves a folder name to its wire identity.
// GUID/UIDVALIDITY come from the folder record so the service keys
// the same mailbox the IMAP path does.
func (s *Server) ftsMailboxRef(uc *userContext, folder string) (fts.MailboxRef, error) {
	bundle, err := uc.ns(s, "")
	if err != nil {
		return fts.MailboxRef{}, err
	}
	if bundle == nil {
		return fts.MailboxRef{}, errNoMailHome
	}
	f, err := bundle.mbox.Folder(folder, 0)
	if err != nil {
		return fts.MailboxRef{}, err
	}
	return fts.MailboxRef{
		Name:        f.Name,
		GUID:        hex.EncodeToString(f.GUID[:]),
		UIDValidity: f.UIDValidity,
	}, nil
}

type ftsStatusResponse struct {
	User             string `json:"user"`
	Folder           string `json:"folder"`
	LastIndexedUID   uint32 `json:"last_indexed_uid"`
	SettingsChecksum uint32 `json:"settings_checksum"`
	// Whole-user, not per folder: one index holds every folder now (#1986).
	Documents uint64 `json:"documents"`
	Copies    uint64 `json:"copies"`
	Messages  uint64 `json:"messages"`
	// UnrecordedCopies are live copies with no row in the GUID store: a hit
	// resolves through it, so such a copy cannot be found (#2031).
	UnrecordedCopies uint64 `json:"unrecorded_copies"`
}

// handleFTSStatus reports the per-mailbox indexing checkpoint.
// GET /api/backend/fts/status?user=&folder=
func (s *Server) handleFTSStatus(w http.ResponseWriter, r *http.Request) {
	if s.opts.FTSClient == nil {
		apiError(w, "fts not configured on this backend-api", http.StatusNotImplemented)
		return
	}
	user := r.URL.Query().Get("user")
	folder := r.URL.Query().Get("folder")
	if user == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return
	}
	if folder == "" {
		apiError(w, errFolderRequired.Error(), http.StatusBadRequest)
		return
	}
	uc, err := s.openUserContextReadOnly(user)
	if err != nil {
		apiError(w, "fts status: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer uc.Close()
	mbox, err := s.ftsMailboxRef(uc, folder)
	if err != nil {
		apiError(w, "fts status: "+err.Error(), http.StatusInternalServerError)
		return
	}
	last, checksum, err := s.opts.FTSClient.Status(user, mbox)
	if err != nil {
		apiError(w, "fts status: "+err.Error(), http.StatusBadGateway)
		return
	}
	docs, copies, messages, unrecorded, err := s.opts.FTSClient.Counts(user)
	if err != nil {
		apiError(w, "fts status: "+err.Error(), http.StatusBadGateway)
		return
	}
	apiJSON(w, ftsStatusResponse{
		User: user, Folder: folder,
		LastIndexedUID: last, SettingsChecksum: checksum,
		Documents: docs, Copies: copies, Messages: messages, UnrecordedCopies: unrecorded,
	})
}

type ftsRescanResponse struct {
	User    string   `json:"user"`
	Folders []string `json:"folders"` // folders that were rescanned
}

// handleFTSRescan reconciles the index against the mailbox. With ?folder=
// only that folder is rescanned; without it, every selectable folder is.
// POST /api/backend/fts/rescan?user=&folder=
func (s *Server) handleFTSRescan(w http.ResponseWriter, r *http.Request) {
	if s.opts.FTSClient == nil {
		apiError(w, "fts not configured on this backend-api", http.StatusNotImplemented)
		return
	}
	user := r.URL.Query().Get("user")
	folder := r.URL.Query().Get("folder")
	if user == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return
	}
	uc, err := s.openUserContextReadOnly(user)
	if err != nil {
		apiError(w, "fts rescan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer uc.Close()

	// Without a folder the walk belongs to the service, which holds the
	// user's index once for all of them instead of once per call (#1986).
	if folder == "" {
		// The mail home is still checked here: an account that has none is
		// refused before anything opens it, never made by the attempt.
		bundle, err := uc.ns(s, "")
		if err != nil {
			apiError(w, "fts rescan: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if bundle == nil {
			apiError(w, errNoMailHome.Error(), http.StatusNotFound)
			return
		}
		done, err := s.opts.FTSClient.RescanUser(user)
		if err != nil {
			apiError(w, "fts rescan: "+err.Error(), http.StatusBadGateway)
			return
		}
		apiJSON(w, ftsRescanResponse{User: user, Folders: done})
		return
	}

	mbox, err := s.ftsMailboxRef(uc, folder)
	if err != nil {
		apiError(w, "fts rescan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.opts.FTSClient.Rescan(user, mbox); err != nil {
		apiError(w, "fts rescan "+folder+": "+err.Error(), http.StatusBadGateway)
		return
	}
	apiJSON(w, ftsRescanResponse{User: user, Folders: []string{folder}})
}

// handleFTSOptimize compacts every index owned by the user.
// POST /api/backend/fts/optimize?user=
func (s *Server) handleFTSOptimize(w http.ResponseWriter, r *http.Request) {
	if s.opts.FTSClient == nil {
		apiError(w, "fts not configured on this backend-api", http.StatusNotImplemented)
		return
	}
	user := r.URL.Query().Get("user")
	if user == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return
	}
	if err := s.opts.FTSClient.Optimize(user); err != nil {
		apiError(w, "fts optimize: "+err.Error(), http.StatusBadGateway)
		return
	}
	apiJSON(w, map[string]string{"user": user, "status": "ok"})
}

type ftsLookupTerm struct {
	Field  string     `json:"field"`
	Header string     `json:"header,omitempty"`
	Words  [][]string `json:"words"`
	Phrase string     `json:"phrase,omitempty"`
}

type ftsLookupResponse struct {
	User   string          `json:"user"`
	Folder string          `json:"folder"`
	Terms  []ftsLookupTerm `json:"terms"`
	// Impossible: a criterion expanded to stopwords only, so nothing can match
	// and the index was not asked.
	Impossible bool     `json:"impossible"`
	Definite   []uint32 `json:"definite"`
	Maybe      []uint32 `json:"maybe"`
}

// handleFTSLookup asks the index what SEARCH would, with the query built as
// SEARCH builds it, so an empty answer is the index's and not the session's.
// GET /api/backend/fts/lookup?user=&folder=&header=NAME:VALUE&body=&text=
func (s *Server) handleFTSLookup(w http.ResponseWriter, r *http.Request) {
	if s.opts.FTSClient == nil || s.opts.FTSChain == nil {
		apiError(w, "fts not configured on this backend-api", http.StatusNotImplemented)
		return
	}
	q := r.URL.Query()
	user, folder := q.Get("user"), q.Get("folder")
	if user == "" {
		apiError(w, errUserRequired.Error(), http.StatusBadRequest)
		return
	}
	if folder == "" {
		apiError(w, errFolderRequired.Error(), http.StatusBadRequest)
		return
	}
	c := ftsquery.Criteria{Body: q["body"], Text: q["text"]}
	for _, h := range q["header"] {
		name, value, ok := strings.Cut(h, ":")
		if !ok || name == "" {
			apiError(w, "header must be NAME:VALUE, got "+h, http.StatusBadRequest)
			return
		}
		c.Header = append(c.Header, ftsquery.Header{Key: name, Value: value})
	}
	if len(c.Body)+len(c.Text)+len(c.Header) == 0 {
		apiError(w, "fts lookup: give at least one of header, body, text", http.StatusBadRequest)
		return
	}
	query, impossible := ftsquery.Build(s.opts.FTSChain, c)
	out := ftsLookupResponse{User: user, Folder: folder, Impossible: impossible,
		Terms: lookupTerms(query), Definite: []uint32{}, Maybe: []uint32{}}
	if impossible {
		apiJSON(w, out)
		return
	}
	uc, err := s.openUserContextReadOnly(user)
	if err != nil {
		apiError(w, "fts lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer uc.Close()
	mbox, err := s.ftsMailboxRef(uc, folder)
	if err != nil {
		apiError(w, "fts lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	res, err := s.opts.FTSClient.Lookup(user, mbox, query)
	if err != nil {
		apiError(w, "fts lookup: "+err.Error(), http.StatusBadGateway)
		return
	}
	out.Definite = append(out.Definite, res.Definite...)
	out.Maybe = append(out.Maybe, res.Maybe...)
	apiJSON(w, out)
}

func lookupTerms(q fts.Query) []ftsLookupTerm {
	fields := map[fts.FieldKind]string{fts.FieldBody: "body", fts.FieldText: "text", fts.FieldHeader: "header"}
	out := make([]ftsLookupTerm, 0, len(q.Terms))
	for _, t := range q.Terms {
		lt := ftsLookupTerm{Field: fields[t.Field], Header: t.HdrName, Phrase: t.Phrase, Words: [][]string{}}
		for _, w := range t.Words {
			lt.Words = append(lt.Words, w.Variants)
		}
		out = append(out, lt)
	}
	return out
}
