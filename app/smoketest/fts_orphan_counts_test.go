package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The judgement the copies row ends on: documents outrunning the live messages
// is a folder whose documents stayed behind, and it must fail the run (#2022).
func TestAssertNoOrphanDocuments(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		docs, copies, msgs, afterCompaction int
		wantErr                             bool
	}{
		{"healthy", 5, 7, 5, 5, false},
		{"copies above messages is normal", 5, 9, 5, 5, false},
		{"a compaction clears what was left", 7, 5, 5, 5, false},
		{"a compaction that clears nothing", 7, 5, 5, 7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			compacted := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if strings.Contains(r.URL.Path, "/optimize") {
					compacted = true
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"user":"u@test"}`)
					return
				}
				docs := tc.docs
				if compacted {
					docs = tc.afterCompaction
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"documents":%d,"copies":%d,"messages":%d}`, docs, tc.copies, tc.msgs)
			}))
			defer srv.Close()
			setFlag(t, flagBackendAPI, srv.URL)
			setFlag(t, &orphanCountWait, 300*time.Millisecond)

			var said bool
			logged(t, &said)
			err := assertNoOrphanDocuments("u@test")
			if said != (tc.docs > tc.msgs) {
				t.Errorf("the line about a compaction was %v, and one was needed: %v", said, tc.docs > tc.msgs)
			}
			if tc.wantErr && err == nil {
				t.Fatalf("%d documents for %d live messages passed a compaction", tc.afterCompaction, tc.msgs)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected failure: %v", err)
			}
		})
	}
}

// logged captures whether the row said a compaction was needed, which is what
// tells the two mechanisms apart in a window's log.
func logged(t *testing.T, said *bool) {
	t.Helper()
	was := slog.Default()
	slog.SetDefault(slog.New(slogHandlerFunc(func(r slog.Record) {
		if strings.Contains(r.Message, "a compaction was needed") {
			*said = true
		}
	})))
	t.Cleanup(func() { slog.SetDefault(was) })
}

type slogHandlerFunc func(slog.Record)

func (f slogHandlerFunc) Enabled(context.Context, slog.Level) bool { return true }
func (f slogHandlerFunc) Handle(_ context.Context, r slog.Record) error {
	f(r)
	return nil
}
func (f slogHandlerFunc) WithAttrs([]slog.Attr) slog.Handler { return f }
func (f slogHandlerFunc) WithGroup(string) slog.Handler      { return f }
