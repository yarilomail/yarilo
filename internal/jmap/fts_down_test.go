package jmap

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/fts"
	"github.com/yarilomail/yarilo/v2/pkg/ftsproto"
)

// When the service cannot answer, a filter of header conditions is answered
// from headers under fts_search_read_fallback, as IMAP scans; text and body,
// and every filter without the fallback, are refused as a retry when the
// service is unreachable (#2480).
func TestEmailQueryWhenFTSCannotAnswer(t *testing.T) {
	unreachable := fmt.Errorf("ftsproto: dial: connection refused: %w", ftsproto.ErrUnavailable)
	tests := []struct {
		name      string
		stub      *stubFTS
		fallback  bool
		filter    string
		wantIDs   int
		wantError string
	}{
		{"unreachable, subject that matches", &stubFTS{statusUID: 1, err: unreachable}, true, `{"subject":"probe"}`, 1, ""},
		{"unreachable, subject that does not", &stubFTS{statusUID: 1, err: unreachable}, true, `{"subject":"absent"}`, 0, ""},
		{"unreachable, from", &stubFTS{statusUID: 1, err: unreachable}, true, `{"from":"alice"}`, 1, ""},
		{"unreachable, header present", &stubFTS{statusUID: 1, err: unreachable}, true, `{"header":["Subject"]}`, 1, ""},
		{"unreachable, text", &stubFTS{statusUID: 1, err: unreachable}, true, `{"text":"probe"}`, 0, "serverUnavailable"},
		{"unreachable, subject without the fallback", &stubFTS{statusUID: 1, err: unreachable}, false, `{"subject":"probe"}`, 0, "serverUnavailable"},
		{"engine refusal, subject", &stubFTS{statusUID: 1, err: errors.New("engine down")}, true, `{"subject":"probe"}`, 1, ""},
		{"engine refusal, text", &stubFTS{statusUID: 1, err: errors.New("engine down")}, true, `{"text":"probe"}`, 0, "serverFail"},
		{"lagging index, subject", &stubFTS{statusUID: 0, byFolder: map[string]fts.Result{}}, true, `{"subject":"probe"}`, 1, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := searchServer(t, tc.stub, 4, 64, map[string]string{"INBOX": "body text"})
			s.opts.FTS.ReadFallback = tc.fallback
			req := `{"accountId":"` + testAccount + `","filter":` + tc.filter + `}`
			if tc.wantError != "" {
				if err := emailQueryError(t, s, req); err["type"] != tc.wantError {
					t.Fatalf("type = %v, want %s", err["type"], tc.wantError)
				}
				return
			}
			got := emailQuery(t, s, req)
			ids, _ := got["ids"].([]any)
			if len(ids) != tc.wantIDs {
				t.Fatalf("ids = %v, want %d", got["ids"], tc.wantIDs)
			}
		})
	}
}
