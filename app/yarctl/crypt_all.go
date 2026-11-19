package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
)

// allResult is one user's job as it ended, or why it did not run.
type allResult struct {
	User string      `json:"user"`
	Job  cryptJobOut `json:"job"`
	Err  string      `json:"error,omitempty"`
}

// allSummary is a --all run: every user's job, the public namespace's, and
// the sums the rotation's last step reads.
type allSummary struct {
	Kind          string         `json:"kind"`
	Users         int            `json:"users"`
	FailedUsers   []string       `json:"failed_users"`
	Totals        map[string]int `json:"totals"`
	Rewritten     int            `json:"rewritten"`
	NoKey         int            `json:"no_key"`
	Failed        int            `json:"failed"`
	FoldersFailed int            `json:"folders_failed"`
	Public        *allResult     `json:"public,omitempty"`
	Results       []allResult    `json:"results"`
}

// cryptAll runs kind for every user user/iterate lists, jobs at a time, each
// on the user's own pod through the usual routing; then the public namespace
// once. rate is shared between the jobs running at once.
func cryptAll(kind string, base map[string]any, jobs, rate int, publicNS string) error {
	data, err := backendAPIPost("/api/backend/user/iterate", map[string]any{})
	if err != nil {
		return fmt.Errorf("list the users: %w", err)
	}
	var list struct {
		Users []string `json:"users"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("list the users: %w", err)
	}
	if jobs < 1 {
		jobs = 1
	}
	perJob := 0
	if rate > 0 {
		perJob = max(1, rate/jobs)
	}

	run := &allRun{running: map[string]bool{}}
	stop := make(chan os.Signal, 2)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-stop:
				if !run.interrupt() {
					names := run.names()
					fmt.Fprintf(os.Stderr, "interrupted twice: leaving now; these jobs may still be running: %s\n", strings.Join(names, ", "))
					exitAll(130)
				}
			case <-done:
				return
			}
		}
	}()

	work := make(chan string)
	results := make(chan allResult)
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range work {
				if !run.begin(u) {
					results <- allResult{User: u, Err: "not started: interrupted"}
					continue
				}
				res := runOneJob(kind, u, base, perJob, "", run.started)
				run.end(u)
				results <- res
			}
		}()
	}
	go func() {
		for _, u := range list.Users {
			work <- u
		}
		close(work)
		wg.Wait()
		close(results)
	}()

	sum := allSummary{Kind: kind, Users: len(list.Users), FailedUsers: []string{}, Totals: map[string]int{}}
	for res := range results {
		sum.add(res)
		fmt.Fprintf(os.Stderr, "%s: %s\n", res.User, res.line())
	}
	if publicNS != "" && len(list.Users) > 0 && run.begin(list.Users[0]) {
		res := runOneJob(kind, list.Users[0], base, rate, publicNS, run.started)
		run.end(list.Users[0])
		if strings.Contains(res.Err, "not configured") {
			fmt.Fprintf(os.Stderr, "namespace %s: not configured, skipped\n", publicNS)
		} else {
			res.User = "namespace " + publicNS
			sum.Public = &res
			sum.add(res)
			fmt.Fprintf(os.Stderr, "%s: %s\n", res.User, res.line())
		}
	}
	sort.Slice(sum.Results, func(i, j int) bool { return sum.Results[i].User < sum.Results[j].User })
	out, _ := json.Marshal(sum)
	if err := printOutput(out, nil, printAllSummary); err != nil {
		return err
	}
	switch {
	case run.wasInterrupted():
		return errors.New("interrupted: the running jobs were cancelled")
	case len(sum.FailedUsers) > 0:
		return fmt.Errorf("crypt %s --all: %d of %d failed: %s", kind, len(sum.FailedUsers), sum.Users, strings.Join(sum.FailedUsers, ", "))
	case sum.Failed > 0 || sum.FoldersFailed > 0:
		return fmt.Errorf("crypt %s --all: %d messages and %d folders failed (named in the backend-api logs)", kind, sum.Failed, sum.FoldersFailed)
	}
	return nil
}

// allRun is what an interrupt needs: the users whose job may be running, and
// whether the run was interrupted.
type allRun struct {
	mu          sync.Mutex
	running     map[string]bool
	interrupted bool
}

// exitAll ends yarctl on a second interrupt. Test seam.
var exitAll = os.Exit

func cancelJob(user string) {
	_, _ = backendAPIDelete("/api/backend/crypt/job?user=" + url.QueryEscape(user))
}

// begin claims user for a job, false once the run was interrupted.
func (r *allRun) begin(user string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.interrupted {
		return false
	}
	r.running[user] = true
	return true
}

// started runs once the user's job exists: a cancel sent before it did met
// nothing, so an interrupt that came meanwhile is sent again now.
func (r *allRun) started(user string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.interrupted {
		cancelJob(user)
	}
}

func (r *allRun) end(user string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, user)
}

// interrupt cancels every running job and reports true, the first time; a
// second interrupt reports false.
func (r *allRun) interrupt() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.interrupted {
		return false
	}
	r.interrupted = true
	for u := range r.running {
		cancelJob(u)
	}
	return true
}

func (r *allRun) wasInterrupted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interrupted
}

func (r *allRun) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.running))
	for u := range r.running {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// runOneJob starts kind for user (in namespace ns, or the user's own when
// empty), tells started once the job exists, and waits for it to end.
func runOneJob(kind, user string, base map[string]any, rate int, ns string, started func(string)) allResult {
	req := map[string]any{}
	for k, v := range base {
		req[k] = v
	}
	req["user"] = user
	if ns != "" {
		req["namespace"] = ns
	}
	if kind == "reencrypt" {
		req["rate"] = rate
	}
	if _, err := backendAPIPost("/api/backend/crypt/"+kind, req); err != nil {
		return allResult{User: user, Err: err.Error()}
	}
	if started != nil {
		started(user)
	}
	path := "/api/backend/crypt/job?user=" + url.QueryEscape(user) + "&wait=30s"
	for {
		data, err := backendAPIGet(path)
		if err != nil {
			return allResult{User: user, Err: "the job is gone (a backend-api restart loses it; rerun): " + err.Error()}
		}
		var j cryptJobOut
		if err := json.Unmarshal(data, &j); err != nil {
			return allResult{User: user, Err: err.Error()}
		}
		if j.State != "running" {
			res := allResult{User: user, Job: j}
			if err := j.verdict(); err != nil {
				res.Err = err.Error()
			}
			return res
		}
	}
}

func (s *allSummary) add(r allResult) {
	s.Results = append(s.Results, r)
	if r.Err != "" {
		s.FailedUsers = append(s.FailedUsers, r.User)
	}
	for k, v := range r.Job.Totals {
		s.Totals[k] += v
	}
	s.Rewritten += r.Job.Rewritten
	s.NoKey += r.Job.NoKey
	s.Failed += r.Job.Failed
	s.FoldersFailed += r.Job.FoldersFailed
}

func (r allResult) line() string {
	if r.Job.State == "" {
		return "not run: " + r.Err
	}
	parts := []string{r.Job.State}
	for _, st := range cryptStates {
		if n := r.Job.Totals[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", st, n))
		}
	}
	if r.Job.Kind == "reencrypt" && !r.Job.DryRun {
		parts = append(parts, fmt.Sprintf("rewritten %d", r.Job.Rewritten))
	}
	if r.Err != "" {
		parts = append(parts, r.Err)
	}
	return strings.Join(parts, ", ")
}

func printAllSummary(data []byte) error {
	var s allSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "\nALL USERS\t%s\tFAILED\n", strings.ToUpper(strings.Join(cryptStates, "\t")))
	cells := make([]string, len(cryptStates))
	for i, st := range cryptStates {
		cells[i] = fmt.Sprint(s.Totals[st])
	}
	fmt.Fprintf(tw, "%d users\t%s\t%d\n", s.Users, strings.Join(cells, "\t"), s.Failed)
	if err := tw.Flush(); err != nil {
		return err
	}
	if s.Kind == "reencrypt" {
		fmt.Printf("rewritten %d, no key %d", s.Rewritten, s.NoKey)
	}
	if len(s.FailedUsers) > 0 {
		fmt.Printf("; failed: %s", strings.Join(s.FailedUsers, ", "))
	}
	fmt.Println()
	return nil
}
