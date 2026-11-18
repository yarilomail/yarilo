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
}

// cryptFolder is one folder's messages counted by state.
type cryptFolder struct {
	Folder string         `json:"folder"`
	States map[string]int `json:"states"`
	Failed int            `json:"failed"`
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
	// WouldRewrite and NoKey are what a re-encryption does and leaves.
	WouldRewrite int
	NoKey        int
	Failed       int

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
		folders[i] = cryptFolder{Folder: f.Folder, States: copyCounts(f.States), Failed: f.Failed}
	}
	return map[string]any{
		"kind": j.Kind, "user": j.User, "namespace": j.Namespace, "dry_run": j.DryRun, "to": j.To,
		"state": j.State, "error": j.Error, "started": j.Started, "finished": j.Finished,
		"seen": j.Seen, "folders": folders, "totals": copyCounts(j.Totals),
		"would_rewrite": j.WouldRewrite, "no_key": j.NoKey, "failed": j.Failed,
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
		if req.Namespace == "" {
			req.Namespace = "personal"
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
			if req.To == "age" && !s.opts.Crypt.Writes() {
				apiError(w, "to age: storage.mail_crypt.write is off, there is no key to write to", http.StatusBadRequest)
				return
			}
			if !req.DryRun {
				apiError(w, "reencrypt: only --dry-run is implemented in this release", http.StatusNotImplemented)
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
		err = locks.WithLock(ctx, s.opts.Locker, locks.CryptJobKey(req.User), locks.Owner(req.User, "backend-api-crypt"),
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
		"would_rewrite", job.WouldRewrite, "no_key", job.NoKey, "failed", job.Failed,
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

func (s *Server) walkCrypt(ctx context.Context, job *cryptJob, req cryptRequest) error {
	uc, err := s.openUserContextReadOnly(req.User)
	if err != nil {
		return err
	}
	defer uc.Close()
	bundle, err := uc.ns(s, req.Namespace)
	if err != nil {
		return err
	}
	if bundle == nil {
		return errNoMailHome
	}
	if isVirtualNamespace(bundle.spec) {
		return nil // its folders name messages that live in other folders
	}
	rr, ok := mailbox.Driver(bundle.box).(mailbox.RawReader)
	if !ok {
		return fmt.Errorf("crypt: driver %s reads no raw records", mailbox.DriverNameOf(bundle.box))
	}
	entries, err := bundle.box.ListFolders()
	if err != nil {
		return fmt.Errorf("crypt: list folders: %w", err)
	}
	for _, e := range entries {
		if len(req.Folders) > 0 && !slices.Contains(req.Folders, e.Name) {
			continue
		}
		folder, err := bundle.mbox.Folder(e.Name, 0)
		if err != nil {
			continue // not selectable: no messages of its own
		}
		msgs, err := bundle.mbox.Messages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
		if err != nil {
			return fmt.Errorf("crypt: %s: %w", e.Name, err)
		}
		row := job.addFolder(e.Name)
		for _, m := range msgs {
			if err := ctx.Err(); err != nil {
				return err
			}
			b, err := rr.RawRecord(e.Name, m)
			if err != nil {
				if errors.Is(err, mailbox.ErrExpunged) {
					continue
				}
				slog.Warn("backendapi/crypt: message unreadable", "user", req.User, "folder", e.Name, "uid", m.UID, "err", err)
				job.count(row, 0, err, req.To)
				continue
			}
			st, perr := s.opts.Crypt.Probe(b)
			_ = b.Close()
			if perr != nil {
				slog.Warn("backendapi/crypt: message unreadable", "user", req.User, "folder", e.Name, "uid", m.UID, "err", perr)
			}
			job.count(row, st, perr, req.To)
		}
	}
	if len(req.Folders) > 0 {
		return nil
	}
	ur, ok := mailbox.Driver(bundle.box).(mailbox.UnreferencedRawReader)
	if !ok {
		return nil
	}
	row := job.addFolder(unreferencedFolder)
	return ur.RawUnreferenced(func(key uint32, b mailbox.RawBody) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, perr := s.opts.Crypt.Probe(b)
		if perr != nil {
			slog.Warn("backendapi/crypt: record unreadable", "user", req.User, "map_uid", key, "err", perr)
		}
		job.count(row, st, perr, req.To)
		return nil
	})
}

func isVirtualNamespace(spec config.NamespaceConfig) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(spec.Location)), "virtual:")
}

func (j *cryptJob) addFolder(name string) *cryptFolder {
	j.mu.Lock()
	defer j.mu.Unlock()
	f := &cryptFolder{Folder: name, States: map[string]int{}}
	j.Folders = append(j.Folders, f)
	return f
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
