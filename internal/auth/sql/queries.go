package sql

import "github.com/yarilomail/yarilo/v2/pkg/sqlvars"

// substituteVars binds the query's variables as positional parameters (? for
// sqlite and mysql, $N for postgres); the grammar is sqlvars'.
func substituteVars(driver, query, username string, v sqlvars.Vars) (string, []any) {
	return sqlvars.Substitute(driver, query, username, v)
}

func splitUser(u string) (local, domain string) { return sqlvars.SplitUser(u) }
