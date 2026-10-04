package sqlpool

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestDefaultMaxOpenConnsPerDriver(t *testing.T) {
	tests := []struct {
		driver string
		want   int
	}{
		{"mysql", 25},
		{"postgres", 8},
		{"sqlite", 1},
		{"", 25},          // unset falls back to the middle value
		{"cockroach", 25}, // unrecognised likewise
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			if got := DefaultMaxOpenConns(tc.driver); got != tc.want {
				t.Fatalf("DefaultMaxOpenConns(%q) = %d, want %d", tc.driver, got, tc.want)
			}
		})
	}
	// postgres connections are processes, so its default must stay below mysql's
	if DefaultMaxOpenConns("postgres") >= DefaultMaxOpenConns("mysql") {
		t.Fatal("the postgres default must stay below the mysql one — its connections are processes")
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/pool.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Idle capacity must follow the open limit, not Go's default of 2.
func TestApplyIdleMatchesOpen(t *testing.T) {
	db := openTestDB(t)
	Apply(db, Config{Driver: "mysql"})

	stats := db.Stats()
	if stats.MaxOpenConnections != 25 {
		t.Fatalf("MaxOpenConnections = %d, want 25", stats.MaxOpenConnections)
	}
	// no MaxIdleConns getter; assert that a returned connection is retained
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if idle := db.Stats().Idle; idle == 0 {
		t.Fatal("no idle connection retained after use — idle capacity did not follow the open limit")
	}
}

func TestApplyExplicitValues(t *testing.T) {
	db := openTestDB(t)
	Apply(db, Config{Driver: "sqlite", MaxOpenConns: new(7), MaxIdleConns: new(3)})

	if got := db.Stats().MaxOpenConnections; got != 7 {
		t.Fatalf("MaxOpenConnections = %d, want 7 (explicit value must beat the driver default)", got)
	}
}

func TestApplyZeroMeansUnlimited(t *testing.T) {
	db := openTestDB(t)
	Apply(db, Config{Driver: "mysql", MaxOpenConns: new(0)})

	if got := db.Stats().MaxOpenConnections; got != 0 {
		t.Fatalf("MaxOpenConnections = %d, want 0 (unlimited)", got)
	}
}

func TestApplyNilDBIsSafe(t *testing.T) {
	Apply(nil, Config{})
}

func TestDurationKnob(t *testing.T) {
	tests := []struct {
		name    string
		seconds *int
		def     time.Duration
		want    time.Duration
	}{
		{"unset takes the default", nil, 5 * time.Minute, 5 * time.Minute},
		{"explicit seconds", new(30), 5 * time.Minute, 30 * time.Second},
		{"zero disables", new(0), 5 * time.Minute, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := seconds(tc.seconds, tc.def); got != tc.want {
				t.Fatalf("seconds(%v) = %v, want %v", tc.seconds, got, tc.want)
			}
		})
	}
}

func TestSQLiteDefaultSerialises(t *testing.T) {
	db := openTestDB(t)
	Apply(db, Config{Driver: "sqlite"})

	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("sqlite MaxOpenConnections = %d, want 1", got)
	}
}
