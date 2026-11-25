package mailops

import (
	"bytes"
	"log/slog"
	"sync"
)

// syncLog is a log sink written by the save while the test reads it.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func slogDefault() *slog.Logger { return slog.Default() }

func setSlog(w *syncLog) { slog.SetDefault(slog.New(slog.NewTextHandler(w, nil))) }

func restoreSlog(l *slog.Logger) { slog.SetDefault(l) }
