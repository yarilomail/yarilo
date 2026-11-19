package backendapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/yarilomail/yarilo/v2/internal/mailcrypt"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// unreferencedFolder is the row a store's unreferenced records are counted
// under: expunged, not purged, and still under whatever key sealed them.
const unreferencedFolder = "(expunged, not purged)"

const (
	cryptJobLease = 60 * time.Second
	cryptJobRenew = 20 * time.Second
	// cryptJobKeep is how long a finished job stays readable.
	cryptJobKeep = time.Hour
)

func (s *Server) registerCryptRoutes() {
	s.mux.Handle("GET /api/backend/crypt/keys", s.middleware(s.handleCryptKeys))
	s.mux.Handle("POST /api/backend/crypt/status", s.middleware(s.handleCryptStart(cryptStatus)))
	s.mux.Handle("POST /api/backend/crypt/reencrypt", s.middleware(s.handleCryptStart(cryptReencrypt)))
	s.mux.Handle("GET /api/backend/crypt/job", s.middleware(s.handleCryptJob))
	s.mux.Handle("DELETE /api/backend/crypt/job", s.middleware(s.handleCryptCancel))
}

func (s *Server) handleCryptKeys(w http.ResponseWriter, _ *http.Request) {
	apiJSON(w, s.opts.Crypt.Keys())
}

type cryptKind string

const (
	cryptStatus    cryptKind = "status"
	cryptReencrypt cryptKind = "reencrypt"
)

type cryptRequest struct {
	User      string   `json:"user"`
	Namespace string   `json:"namespace"`
	Folders   []string `json:"folders"`
	DryRun    bool     `json:"dry_run"`
	To        string   `json:"to"`
	// Rate is the most messages a second this job rewrites; 0 is no bound.
	Rate int `json:"rate"`
}

// cryptFolder is one folder's messages counted by state.
type cryptFolder struct {
	Namespace string         `json:"namespace"`
	Folder    string         `json:"folder"`
	States    map[string]int `json:"states"`
	Failed    int            `json:"failed"`
	Error     string         `json:"error,omitempty"`
}

// cryptJob is one user's status or re-encryption run. It lives in this
// process only: a restart loses it, and a rerun classifies every message again.
type cryptJob struct {
	mu        sync.Mutex
	Kind      cryptKind
	User      string
	Namespace string
	DryRun    bool
	To        string
	State     string // running, done, failed, cancelled
	Error     string
	Started   time.Time
	Finished  *time.Time
	Seen      int
	Folders   []*cryptFolder
	Totals    map[string]int
	// WouldRewrite and NoKey are what a re-encryption does and leaves;
	// Rewritten and Skipped (expunged meanwhile) what a real run did.
	WouldRewrite int
	Rewritten    int
	Skipped      int
	NoKey        int
	Failed       int
	// FoldersFailed are folders that could not be opened.
	FoldersFailed int

	cancel context.CancelFunc
	done   chan struct{} // closed when the job ends
}

type cryptJobs struct {
	mu     sync.Mutex
	byUser map[string]*cryptJob
}

func (j *cryptJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	folders := make([]cryptFolder, len(j.Folders))
	for i, f := range j.Folders {
		folders[i] = cryptFolder{Namespace: f.Namespace, Folder: f.Folder, States: copyCounts(f.States), Failed: f.Failed, Error: f.Error}
	}
	return map[string]any{
		"kind": j.Kind, "user": j.User, "namespace": j.Namespace, "dry_run": j.DryRun, "to": j.To,
		"state": j.State, "error": j.Error, "started": j.Started, "finished": j.Finished,
		"seen": j.Seen, "folders": folders, "totals": copyCounts(j.Totals),
		"would_rewrite": j.WouldRewrite, "rewritten": j.Rewritten, "skipped": j.Skipped,
		"no_key": j.NoKey, "failed": j.Failed,
		"folders_failed": j.FoldersFailed,
	}
}

func copyCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *Server) handleCryptStart(kind cryptKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req cryptRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.User == "" {
			apiError(w, "user is required", http.StatusBadRequest)
			return
		}
		if kind == cryptReencrypt {
			switch req.To {
			case "":
				req.To = "age"
			case "age", "plain":
			default:
				apiError(w, fmt.Sprintf("to: %q is neither age nor plain", req.To), http.StatusBadRequest)
				return
			}
			// The sessions are other processes: without the lock service a
			// swap would be excluded from nothing but this process.
			if !req.DryRun && s.opts.Locker == nil {
				apiError(w, "reencrypt: backend-api runs without the lock service, and a rewrite needs it; --dry-run works", http.StatusServiceUnavailable)
				return
			}
			if req.Rate < 0 {
				apiError(w, "rate: a negative rate means nothing", http.StatusBadRequest)
				return
			}
			if req.To == "age" && !s.opts.Crypt.Writes() {
				apiError(w, "to age: storage.mail_crypt.write is off, there is no key to write to", http.StatusBadRequest)
				return
			}
		}
		job, err := s.startCryptJob(kind, req)
		if err != nil {
			apiError(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		apiJSON(w, job.snapshot())
	}
}

// handleCryptJob answers at once, or with ?wait=<duration> when the job ends
// or the wait runs out, whichever is first.
func (s *Server) handleCryptJob(w http.ResponseWriter, r *http.Request) {
	job := s.cryptJobFor(r.URL.Query().Get("user"))
	if job == nil {
		apiError(w, "no crypt job for this user on this pod", http.StatusNotFound)
		return
	}
	if v := r.URL.Query().Get("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || d > time.Minute {
			apiError(w, "wait: a duration up to 1m", http.StatusBadRequest)
			return
		}
		select {
		case <-job.done:
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	apiJSON(w, job.snapshot())
}

func (s *Server) handleCryptCancel(w http.ResponseWriter, r *http.Request) {
	job := s.cryptJobFor(r.URL.Query().Get("user"))
	if job == nil {
		apiError(w, "no crypt job for this user on this pod", http.StatusNotFound)
		return
	}
	job.cancel()
	apiJSON(w, job.snapshot())
}

func (s *Server) cryptJobFor(user string) *cryptJob {
	s.crypt.mu.Lock()
	defer s.crypt.mu.Unlock()
	return s.crypt.byUser[user]
}

// startCryptJob refuses a second job for a user this process is running; the
// lease refuses one another process runs.
func (s *Server) startCryptJob(kind cryptKind, req cryptRequest) (*cryptJob, error) {
	s.crypt.mu.Lock()
	defer s.crypt.mu.Unlock()
	if s.crypt.byUser == nil {
		s.crypt.byUser = map[string]*cryptJob{}
	}
	if old := s.crypt.byUser[req.User]; old != nil {
		old.mu.Lock()
		running := old.State == "running"
		old.mu.Unlock()
		if running {
			return nil, fmt.Errorf("a crypt %s job for %s is running", old.Kind, req.User)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &cryptJob{
		Kind: kind, User: req.User, Namespace: req.Namespace, DryRun: req.DryRun, To: req.To,
		State: "running", Started: time.Now(), Totals: map[string]int{}, cancel: cancel,
		done: make(chan struct{}),
	}
	s.crypt.byUser[req.User] = job
	go s.runCryptJob(ctx, job, req)
	return job, nil
}

func (s *Server) runCryptJob(ctx context.Context, job *cryptJob, req cryptRequest) {
	defer job.cancel()
	run := func(ctx context.Context) error { return s.walkCrypt(ctx, job, req) }
	var err error
	if s.opts.Locker != nil {
		err = locks.WithLock(locks.WithSite(ctx, "crypt-job"), s.opts.Locker, locks.CryptJobKey(req.User), locks.Owner(req.User, "backend-api-crypt"),
			cryptJobLease, cryptJobRenew, run)
	} else {
		err = run(ctx)
	}
	now := time.Now()
	job.mu.Lock()
	job.Finished = &now
	switch {
	case errors.Is(err, context.Canceled):
		job.State = "cancelled"
	case err != nil:
		job.State, job.Error = "failed", err.Error()
	default:
		job.State = "done"
	}
	line := []any{"user", job.User, "kind", job.Kind, "state", job.State, "seen", job.Seen,
		"would_rewrite", job.WouldRewrite, "rewritten", job.Rewritten, "skipped", job.Skipped, "no_key", job.NoKey, "failed", job.Failed, "folders_failed", job.FoldersFailed,
		"duration_ms", now.Sub(job.Started).Milliseconds()}
	job.mu.Unlock()
	close(job.done)
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("backendapi/crypt: job failed", append(line, "err", err)...)
	} else {
		slog.Info("backendapi/crypt: job finished", line...)
	}
	time.AfterFunc(cryptJobKeep, func() {
		s.crypt.mu.Lock()
		defer s.crypt.mu.Unlock()
		if s.crypt.byUser[job.User] == job {
			delete(s.crypt.byUser, job.User)
		}
	})
}

// needsRewrite is what a re-encryption to "to" rewrites; StateNoKey is left.
func needsRewrite(st mailcrypt.State, to string) bool {
	switch st {
	case mailcrypt.StateOtherKey, mailcrypt.StateReference:
		return true
	case mailcrypt.StatePlain:
		return to == "age"
	case mailcrypt.StateCurrent:
		return to == "plain"
	}
	return false
}

// cryptNamespaces are the namespaces a job walks: the one asked for, or every
// namespace of the user's that has a store of its own.
func (s *Server) cryptNamespaces(asked string) []string {
	if asked != "" {
		return []string{asked}
	}
	out := []string{"personal"}
	for _, spec := range s.opts.Namespaces {
		if s.ownsStore(spec) && !slices.Contains(out, slugFor(spec)) {
			out = append(out, slugFor(spec))
		}
	}
	return out
}

func (s *Server) walkCrypt(ctx context.Context, job *cryptJob, req cryptRequest) error {
	uc, err := s.openUserContextReadOnly(req.User)
	if err != nil {
		return err
	}
	defer uc.Close()
	for _, ns := range s.cryptNamespaces(req.Namespace) {
		if err := s.walkCryptNamespace(ctx, job, req, uc, ns); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) walkCryptNamespace(ctx context.Context, job *cryptJob, req cryptRequest, uc *userContext, ns string) error {
	bundle, err := uc.ns(s, ns)
	if err != nil {
		return err
	}
	if bundle == nil {
		if ns == "personal" {
			return errNoMailHome
		}
		return nil // that namespace's home was never made: no mail in it
	}
	if isVirtualNamespace(bundle.spec) {
		return nil // its folders name messages that live in other folders
	}
	driver := mailbox.DriverNameOf(bundle.box)
	rr, ok := mailbox.Driver(bundle.box).(mailbox.RawReader)
	if !ok {
		return fmt.Errorf("crypt: driver %s reads no raw records", driver)
	}
	writing := job.Kind == cryptReencrypt && !req.DryRun
	var rw mailbox.RecordRewriter
	var sw mailbox.StoreRewriter
	if writing {
		rw, _ = mailbox.Driver(bundle.box).(mailbox.RecordRewriter)
		sw, _ = mailbox.Driver(bundle.box).(mailbox.StoreRewriter)
		if rw == nil && sw == nil {
			return fmt.Errorf("crypt: re-encryption of %s stores is not implemented yet", driver)
		}
	}
	layer := s.opts.Crypt.WithoutCache()
	limiter := jobLimiter(req.Rate)
	entries, err := bundle.box.ListFolders()
	if err != nil {
		return fmt.Errorf("crypt: %s: list folders: %w", ns, err)
	}
	for _, e := range entries {
		if !e.Selectable || (len(req.Folders) > 0 && !slices.Contains(req.Folders, e.Name)) {
			continue
		}
		row := job.addFolder(ns, e.Name)
		if rw != nil {
			if n, err := rw.SweepRewriteTemps(e.Name, 2*cryptJobLease); err != nil {
				return fmt.Errorf("crypt: %s %s: %w", ns, e.Name, err)
			} else if n > 0 {
				slog.Info("backendapi/crypt: removed what an interrupted run left", "user", req.User, "namespace", ns, "folder", e.Name, "files", n)
			}
		}
		folder, err := bundle.mbox.Folder(e.Name, 0)
		var msgs []*mailbox.MessageMeta
		if err == nil {
			msgs, err = bundle.mbox.Messages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
		}
		if err != nil {
			slog.Error("backendapi/crypt: folder unreadable", "user", req.User, "namespace", ns, "folder", e.Name, "err", err)
			job.folderFailed(row, err)
			continue
		}
		for _, m := range msgs {
			if err := ctx.Err(); err != nil {
				return err
			}
			b, err := rr.RawRecord(e.Name, m)
			if err != nil {
				if errors.Is(err, mailbox.ErrExpunged) {
					continue
				}
				slog.Warn("backendapi/crypt: message unreadable", "user", req.User, "namespace", ns, "folder", e.Name, "uid", m.UID, "err", err)
				job.count(row, 0, err, req.To)
				continue
			}
			st, perr := layer.Probe(b)
			_ = b.Close()
			if perr != nil {
				slog.Warn("backendapi/crypt: message unreadable", "user", req.User, "namespace", ns, "folder", e.Name, "uid", m.UID, "err", perr)
			}
			job.count(row, st, perr, req.To)
			if perr != nil || !writing {
				continue
			}
			if st == mailcrypt.StateNoKey {
				mailcrypt.CountReencryptNoKey(driver)
				continue
			}
			if rw == nil {
				continue // rewritten by the store, file by file, below
			}
			if !needsRewrite(st, req.To) {
				continue
			}
			if err := limiter.Wait(ctx); err != nil {
				return err
			}
			uid, folderID := m.UID, folder.ID
			recorded := func() (bool, error) {
				got, err := bundle.mbox.Messages(folderID, mailbox.SeqSet{{From: uid, To: uid}})
				return len(got) == 1, err
			}
			out, err := rewriteBody(layer, rw, driver, req.User, ns, e.Name, m, st, req.To, recorded)
			job.rewrote(row, out)
			if err != nil {
				return err
			}
		}
	}
	if len(req.Folders) > 0 {
		if sw != nil {
			return fmt.Errorf("crypt: %s stores share files between folders, so a run takes the whole store, not chosen folders", driver)
		}
		return nil
	}
	if ur, ok := mailbox.Driver(bundle.box).(mailbox.UnreferencedRawReader); ok {
		if err := s.countUnreferenced(ctx, job, req, ns, ur, layer); err != nil {
			return err
		}
	}
	if sw == nil {
		return nil
	}
	return s.rewriteStore(ctx, job, req, ns, driver, sw, layer, limiter)
}

// jobLimiter is one token a message; a zero rate is no bound.
func jobLimiter(perSecond int) *rate.Limiter {
	if perSecond <= 0 {
		return rate.NewLimiter(rate.Inf, 1)
	}
	return rate.NewLimiter(rate.Limit(perSecond), 1)
}

func (s *Server) countUnreferenced(ctx context.Context, job *cryptJob, req cryptRequest, ns string,
	ur mailbox.UnreferencedRawReader, layer *mailcrypt.Layer) error {
	row := job.addFolder(ns, unreferencedFolder)
	return ur.RawUnreferenced(func(key uint32, b mailbox.RawBody) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, perr := layer.Probe(b)
		if perr != nil {
			slog.Warn("backendapi/crypt: record unreadable", "user", req.User, "namespace", ns, "map_uid", key, "err", perr)
		}
		job.count(row, st, perr, req.To)
		return nil
	})
}

func isVirtualNamespace(spec config.NamespaceConfig) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(spec.Location)), "virtual:")
}

func (j *cryptJob) addFolder(ns, name string) *cryptFolder {
	j.mu.Lock()
	defer j.mu.Unlock()
	f := &cryptFolder{Namespace: ns, Folder: name, States: map[string]int{}}
	j.Folders = append(j.Folders, f)
	return f
}

func (j *cryptJob) rewroteMany(n int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Rewritten += n
}

func (j *cryptJob) rewrote(f *cryptFolder, out rewriteOutcome) {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch out {
	case rewriteDone:
		j.Rewritten++
	case rewriteSkipped:
		j.Skipped++
	case rewriteFailed:
		f.Failed++
		j.Failed++
	}
}

// folderFailed marks a folder that could not be opened: its messages are not
// counted, so the job cannot report a zero for them.
func (j *cryptJob) folderFailed(f *cryptFolder, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f.Error = err.Error()
	j.FoldersFailed++
}

func (j *cryptJob) count(f *cryptFolder, st mailcrypt.State, err error, to string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Seen++
	if err != nil {
		f.Failed++
		j.Failed++
		return
	}
	f.States[st.String()]++
	j.Totals[st.String()]++
	switch {
	case st == mailcrypt.StateNoKey:
		j.NoKey++
	case j.Kind == cryptReencrypt && needsRewrite(st, to):
		j.WouldRewrite++
	}
}
