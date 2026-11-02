package main

import (
	"strings"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/backendapi"
	"github.com/yarilomail/yarilo/v2/internal/fts/language"
	"github.com/yarilomail/yarilo/v2/pkg/fts"
	"github.com/yarilomail/yarilo/v2/pkg/ftsproto"
)

// recordingFTS answers from fixed numbers and keeps what it was asked, so a
// test can see which folder a command reached and what came back from it.
type recordingFTS struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingFTS) note(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (r *recordingFTS) Index(string, fts.MailboxRef, uint32, int) error        { return nil }
func (r *recordingFTS) Prepend(string, fts.MailboxRef, uint32) error           { return nil }
func (r *recordingFTS) Expunge(string, fts.MailboxRef, uint32, [16]byte) error { return nil }
func (r *recordingFTS) DropFolder(string, fts.MailboxRef) error                { return nil }
func (r *recordingFTS) LookupIn(string, []fts.MailboxRef, fts.Query) (fts.SetResult, error) {
	return fts.SetResult{}, nil
}
func (r *recordingFTS) Lookup(_ string, m fts.MailboxRef, _ fts.Query) (fts.Result, error) {
	r.note("lookup " + m.Name)
	return fts.Result{Definite: []uint32{1}, Maybe: []uint32{2}}, nil
}
func (r *recordingFTS) Status(_ string, m fts.MailboxRef) (uint32, uint32, error) {
	r.note("status " + m.Name)
	return 7, 42, nil
}
func (r *recordingFTS) Rescan(_ string, m fts.MailboxRef) error {
	r.note("rescan " + m.Name)
	return nil
}
func (r *recordingFTS) RescanUser(string) ([]string, error) {
	r.note("rescan-user")
	return []string{"INBOX", "Work"}, nil
}
func (r *recordingFTS) Counts(string) (uint64, uint64, uint64, uint64, error) { return 3, 4, 2, 1, nil }
func (r *recordingFTS) Optimize(string) error {
	r.note("optimize")
	return nil
}
func (r *recordingFTS) Close() error { return nil }

var _ ftsproto.Client = (*recordingFTS)(nil)

// Each fts verb reaches the service for the folder it names, and its answer
// comes back to the operator unchanged.
func TestFTSVerbsAgainstTheBackend(t *testing.T) {
	svc := &recordingFTS{}
	chain, err := language.NewMultiChain([]string{"english"}, nil, nil, language.DefaultTokenMaxLen, language.DefaultAddressMaxLen, 10)
	if err != nil {
		t.Fatal(err)
	}
	b := startLiveBackendWith(t, func(o *backendapi.Options) { o.FTSClient, o.FTSChain = svc, chain })
	seedInbox(t, b, map[string]string{"1700000000.M1P1.host:2,S": liveMsg1})
	mustYarctl(t, "backend", "folder", "create", liveUser, "Work")
	outputFormat = "json"

	var st struct {
		Folder         string `json:"folder"`
		LastIndexedUID uint32 `json:"last_indexed_uid"`
		Documents      uint64 `json:"documents"`
		Unrecorded     uint64 `json:"unrecorded_copies"`
	}
	decode(t, mustYarctl(t, "backend", "fts", "status", liveUser, "--folder", "Work"), &st)
	if st.Folder != "Work" || st.LastIndexedUID != 7 || st.Documents != 3 || st.Unrecorded != 1 {
		t.Errorf("fts status: %+v, want Work at UID 7, 3 documents, 1 unrecorded", st)
	}
	var rescan struct {
		Folders []string `json:"folders"`
	}
	decode(t, mustYarctl(t, "backend", "fts", "rescan", liveUser, "--folder", "Work"), &rescan)
	if len(rescan.Folders) != 1 || rescan.Folders[0] != "Work" {
		t.Errorf("fts rescan --folder Work: %v", rescan.Folders)
	}
	decode(t, mustYarctl(t, "backend", "fts", "rescan", liveUser), &rescan)
	if len(rescan.Folders) != 2 {
		t.Errorf("fts rescan of every folder: %v, want what the service did", rescan.Folders)
	}
	mustYarctl(t, "backend", "fts", "optimize", liveUser)
	var hits struct {
		Definite []uint32 `json:"definite"`
		Maybe    []uint32 `json:"maybe"`
	}
	decode(t, mustYarctl(t, "backend", "fts", "lookup", liveUser, "--text", "body"), &hits)
	if len(hits.Definite) != 1 || len(hits.Maybe) != 1 {
		t.Errorf("fts lookup: %+v, want the service's one definite and one maybe", hits)
	}

	want := []string{"status Work", "rescan Work", "rescan-user", "optimize", "lookup INBOX"}
	svc.mu.Lock()
	got := strings.Join(svc.calls, ", ")
	svc.mu.Unlock()
	if got != strings.Join(want, ", ") {
		t.Errorf("the service was asked: %s\nwant: %s", got, strings.Join(want, ", "))
	}

	for _, args := range [][]string{
		{"backend", "fts", "status"},
		{"backend", "fts", "rescan"},
		{"backend", "fts", "optimize"},
	} {
		if _, err := yarctl(t, args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("yarctl %s = %v, want its usage", strings.Join(args, " "), err)
		}
	}
	if _, err := yarctl(t, "backend", "fts", "nosuch"); err == nil {
		t.Error("an unknown fts command was accepted")
	}
}
