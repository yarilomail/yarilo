package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
)

// purgeStats is the backend's answer for one user.
type purgeStats struct {
	Skipped         string `json:"skipped,omitempty"`
	FilesScanned    int    `json:"files_scanned"`
	FilesRewritten  int    `json:"files_rewritten"`
	FilesUnlinked   int    `json:"files_unlinked"`
	RecordsKept     int    `json:"records_kept"`
	RecordsExpunged int    `json:"records_expunged"`
	BytesReclaimed  int64  `json:"bytes_reclaimed"`
}

type purgeResult struct {
	User  string     `json:"user"`
	Stats purgeStats `json:"stats"`
	Err   string     `json:"error,omitempty"`
}

// purgeSummary is a purge over many users: who failed, who was skipped, and
// the sums of what the purged ones reclaimed.
type purgeSummary struct {
	Users        int           `json:"users"`
	Purged       int           `json:"purged"`
	Skipped      int           `json:"skipped"`
	NoHome       int           `json:"no_home"`
	FailedUsers  []string      `json:"failed_users"`
	SkippedUsers []string      `json:"skipped_users"`
	NoHomeUsers  []string      `json:"no_home_users"`
	Totals       purgeStats    `json:"totals"`
	Results      []purgeResult `json:"results"`
}

func iterateUsers() ([]string, error) {
	data, err := backendAPIPost("/api/backend/user/iterate", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("list the users: %w", err)
	}
	var list struct {
		Users []string `json:"users"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("list the users: %w", err)
	}
	return list.Users, nil
}

// readUserFile reads one user a line; blank lines and # comments are skipped.
func readUserFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("user file: %w", err)
	}
	defer f.Close() //nolint:errcheck
	var users []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		users = append(users, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("user file: %w", err)
	}
	return users, nil
}

// purgeMany purges every user, jobs at a time, each on the user's own backend
// through the usual routing. One user's failure does not stop the others.
func purgeMany(users []string, ns string, jobs int) error {
	work := make(chan string)
	results := make(chan purgeResult)
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			purgeWorkerStarted()
			for u := range work {
				results <- purgeOne(u, ns)
			}
		}()
	}
	go func() {
		for _, u := range users {
			work <- u
		}
		close(work)
		wg.Wait()
		close(results)
	}()

	sum := purgeSummary{Users: len(users), FailedUsers: []string{}, SkippedUsers: []string{}, NoHomeUsers: []string{}}
	for res := range results {
		sum.add(res)
		fmt.Fprintf(os.Stderr, "%s: %s\n", res.User, res.line())
	}
	sort.Strings(sum.FailedUsers)
	sort.Strings(sum.SkippedUsers)
	sort.Strings(sum.NoHomeUsers)
	sort.Slice(sum.Results, func(i, j int) bool { return sum.Results[i].User < sum.Results[j].User })
	out, _ := json.Marshal(sum)
	if err := printOutput(out, nil, printPurgeSummary); err != nil {
		return err
	}
	if len(sum.FailedUsers) > 0 {
		return fmt.Errorf("mdbox purge: %d of %d failed: %s", len(sum.FailedUsers), sum.Users, strings.Join(sum.FailedUsers, ", "))
	}
	return nil
}

// noMailHome is the backend's skipped reason for an account without a home.
const noMailHome = "no mail home for this user"

// purgeWorkerStarted is a test seam: it runs once a purge worker starts.
var purgeWorkerStarted = func() {}

func purgeOne(user, ns string) purgeResult {
	data, err := backendAPIPost("/api/backend/mdbox/purge", map[string]any{"user": user, "namespace": ns, "skip_other_drivers": true, "skip_no_home": true})
	if err != nil {
		return purgeResult{User: user, Err: err.Error()}
	}
	var st purgeStats
	if err := json.Unmarshal(data, &st); err != nil {
		return purgeResult{User: user, Err: err.Error()}
	}
	return purgeResult{User: user, Stats: st}
}

func (s *purgeSummary) add(r purgeResult) {
	s.Results = append(s.Results, r)
	switch {
	case r.Err != "":
		s.FailedUsers = append(s.FailedUsers, r.User)
	case r.Stats.Skipped == noMailHome:
		s.NoHome++
		s.NoHomeUsers = append(s.NoHomeUsers, r.User)
	case r.Stats.Skipped != "":
		s.Skipped++
		s.SkippedUsers = append(s.SkippedUsers, r.User)
	default:
		s.Purged++
		s.Totals.FilesScanned += r.Stats.FilesScanned
		s.Totals.FilesRewritten += r.Stats.FilesRewritten
		s.Totals.FilesUnlinked += r.Stats.FilesUnlinked
		s.Totals.RecordsKept += r.Stats.RecordsKept
		s.Totals.RecordsExpunged += r.Stats.RecordsExpunged
		s.Totals.BytesReclaimed += r.Stats.BytesReclaimed
	}
}

func (r purgeResult) line() string {
	switch {
	case r.Err != "":
		return "failed: " + r.Err
	case r.Stats.Skipped != "":
		return "skipped: " + r.Stats.Skipped
	}
	return fmt.Sprintf("rewritten %d, unlinked %d, expunged %d, reclaimed %d B",
		r.Stats.FilesRewritten, r.Stats.FilesUnlinked, r.Stats.RecordsExpunged, r.Stats.BytesReclaimed)
}

func printPurgeSummary(data []byte) error {
	var s purgeSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nUSERS\tPURGED\tNOT MDBOX\tNO HOME\tFAILED\tREWRITTEN\tUNLINKED\tEXPUNGED\tRECLAIMED")
	fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d B\n", s.Users, s.Purged, s.Skipped, s.NoHome, len(s.FailedUsers),
		s.Totals.FilesRewritten, s.Totals.FilesUnlinked, s.Totals.RecordsExpunged, s.Totals.BytesReclaimed)
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(s.FailedUsers) > 0 {
		fmt.Printf("failed: %s\n", strings.Join(s.FailedUsers, ", "))
	}
	return nil
}
