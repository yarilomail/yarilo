// Package passdbs builds passdb and userdb chains from config entries,
// dispatching on the driver name. It is the single place that knows which
// concrete backend a driver string maps to, shared by the in-process backend
// wiring and the standalone yarilo-auth process so the two never drift.
package passdbs

import (
	"fmt"
	"strings"

	"github.com/yarilomail/yarilo/internal/auth/passwdfile"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
	authsql "github.com/yarilomail/yarilo/internal/auth/sql"
	"github.com/yarilomail/yarilo/internal/auth/static"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/sqlpool"
)

// Build turns passdb config entries into a passdb chain and a parallel userdb
// chain. A driver that can serve userdb lookups (SQL with a user_query,
// passwd-file) contributes to both; a passdb-only driver contributes to the
// passdb chain alone. Entry order is preserved so chain precedence matches the
// config.
func Build(entries []config.PassdbEntry) (passdbs []protocol.Passdb, userdbs []protocol.Userdb, err error) {
	var statics []*static.DB
	for _, e := range entries {
		switch strings.ToLower(e.Driver) {
		case "sqlite", "mysql", "postgres":
			cfg := authsql.Config{
				Driver:            e.Driver,
				DSN:               e.DSN,
				PasswordQuery:     e.PasswordQuery,
				UserQuery:         e.UserQuery,
				IterateQuery:      e.IterateQuery,
				DefaultPassScheme: e.DefaultPassScheme,
				SkipSchema:        e.SkipSchema,
				UsernameFilter:    protocol.ParseUsernameFilter(e.UsernameFilter),
				Pool: sqlpool.Config{
					MaxOpenConns:           e.MaxOpenConns,
					MaxIdleConns:           e.MaxIdleConns,
					ConnMaxLifetimeSeconds: e.ConnMaxLifetime,
					ConnMaxIdleTimeSeconds: e.ConnMaxIdleTime,
				},
			}
			pdb, err := authsql.New(cfg)
			if err != nil {
				return nil, nil, fmt.Errorf("passdb %s: %w", e.Driver, err)
			}
			passdbs = append(passdbs, pdb)
			udb, err := authsql.NewUserdb(cfg)
			if err != nil {
				return nil, nil, fmt.Errorf("userdb %s: %w", e.Driver, err)
			}
			userdbs = append(userdbs, udb)
		case "passwd-file":
			db, err := passwdfile.New(passwdfile.Config{
				Path:           e.PasswdFile,
				DefaultScheme:  e.DefaultPassScheme,
				UsernameFilter: protocol.ParseUsernameFilter(e.UsernameFilter),
			})
			if err != nil {
				return nil, nil, fmt.Errorf("passdb passwd-file: %w", err)
			}
			passdbs = append(passdbs, db)
			userdbs = append(userdbs, db)
		case "static":
			db, err := static.New(static.Config{
				Password:       e.StaticPassword,
				Nopassword:     e.Nopassword,
				DefaultScheme:  e.DefaultPassScheme,
				Fields:         e.Fields,
				UsernameFilter: protocol.ParseUsernameFilter(e.UsernameFilter),
				AllowAllUsers:  e.StaticAllowAllUsers,
			})
			if err != nil {
				return nil, nil, fmt.Errorf("passdb static: %w", err)
			}
			passdbs = append(passdbs, db)
			userdbs = append(userdbs, db)
			statics = append(statics, db)
		default:
			return nil, nil, fmt.Errorf("unknown passdb driver: %s", e.Driver)
		}
	}
	// A static userdb checks a name against the whole passdb chain, its own
	// entry included, as the reference's lookup_credentials walks every passdb.
	for _, s := range statics {
		s.SetUserExists(func(username string) (bool, error) { return protocol.UserExists(passdbs, username) })
	}
	return passdbs, userdbs, nil
}
