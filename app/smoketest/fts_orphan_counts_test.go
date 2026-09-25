package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The judgement the copies row ends on: documents outrunning the live messages
// is a folder whose documents stayed behind, and it must fail the run (#2022).
func TestAssertNoOrphanDocuments(t *testing.T) {
	for _, tc := range []struct {
		name               string
		docs, copies, msgs int
		wantErr            bool
	}{
		{"healthy", 5, 7, 5, false},
		{"copies above messages is normal", 5, 9, 5, false},
		{"a deleted folder left its documents", 7, 5, 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"documents":%d,"copies":%d,"messages":%d}`, tc.docs, tc.copies, tc.msgs)
			}))
			defer srv.Close()
			setFlag(t, flagBackendAPI, srv.URL)
			setFlag(t, &orphanCountWait, 300*time.Millisecond)

			err := assertNoOrphanDocuments("u@test")
			if tc.wantErr && err == nil {
				t.Fatalf("%d documents for %d live messages passed", tc.docs, tc.msgs)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected failure: %v", err)
			}
		})
	}
}
