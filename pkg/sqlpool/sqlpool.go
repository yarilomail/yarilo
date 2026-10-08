// Package sqlpool applies connection-pool limits to a database/sql handle.
// Go's default MaxIdleConns of 2 causes connection churn under bursts.
package sqlpool

import (
	"database/sql"
	"time"
)

const (
	// DefaultConnMaxLifetime recycles connections so they do not pin to a failed-over server.
	DefaultConnMaxLifetime = 5 * time.Minute
	// DefaultConnMaxIdleTime returns capacity to the server when the process goes quiet.
	DefaultConnMaxIdleTime = time.Minute
)

// Per-driver open-connection defaults; connection cost differs widely between drivers.
const (
	// MySQL connections are threads; stock max_connections is 151.
	mysqlMaxOpenConns = 25

	// Postgres forks a process per connection; stock max_connections is 100.
	postgresMaxOpenConns = 8

	// SQLite writers serialise inside the library; extra connections only produce SQLITE_BUSY.
	sqliteMaxOpenConns = 1
)

// DefaultMaxOpenConns returns the open-connection limit for a driver name
// ("mysql" | "postgres" | "sqlite"). Unrecognised names get the MySQL value.
func DefaultMaxOpenConns(driver string) int {
	switch driver {
	case "postgres":
		return postgresMaxOpenConns
	case "sqlite":
		return sqliteMaxOpenConns
	default:
		return mysqlMaxOpenConns
	}
}

// Config carries pool limits. nil takes the default; 0 lifts the limit.
type Config struct {
	// Driver selects the per-driver default for MaxOpenConns: "mysql" |
	// "postgres" | "sqlite".
	Driver string
	// MaxOpenConns caps total connections (in use plus idle); 0 is unlimited.
	MaxOpenConns *int
	// MaxIdleConns caps retained idle connections; nil mirrors MaxOpenConns,
	// 0 keeps none.
	MaxIdleConns *int
	// ConnMaxLifetimeSeconds recycles a connection after this age; 0 never.
	ConnMaxLifetimeSeconds *int
	// ConnMaxIdleTimeSeconds closes a connection idle this long; 0 never.
	ConnMaxIdleTimeSeconds *int
}

// Apply configures db in place. nil db is a no-op.
func Apply(db *sql.DB, c Config) {
	if db == nil {
		return
	}
	maxOpen := DefaultMaxOpenConns(c.Driver)
	if c.MaxOpenConns != nil {
		maxOpen = *c.MaxOpenConns // database/sql: 0 means unlimited
	}
	db.SetMaxOpenConns(maxOpen)

	// Mirror the open limit rather than Go's 2, or the difference is
	// re-dialled on every burst.
	maxIdle := maxOpen
	if maxIdle == 0 {
		maxIdle = DefaultMaxOpenConns(c.Driver)
	}
	if c.MaxIdleConns != nil {
		maxIdle = *c.MaxIdleConns
	}
	db.SetMaxIdleConns(maxIdle)

	db.SetConnMaxLifetime(seconds(c.ConnMaxLifetimeSeconds, DefaultConnMaxLifetime))
	db.SetConnMaxIdleTime(seconds(c.ConnMaxIdleTimeSeconds, DefaultConnMaxIdleTime))
}

// seconds is the setting as a duration, def when unset; 0 is "never" to database/sql.
func seconds(v *int, def time.Duration) time.Duration {
	if v == nil {
		return def
	}
	return time.Duration(*v) * time.Second
}
