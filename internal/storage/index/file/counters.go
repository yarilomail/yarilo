package file

import (
	"log/slog"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
)

// siteMoves names the data types a successful write at each site changes; a
// site absent here writes nothing a client can see.
var siteMoves = map[string][]counters.Type{
	lockSiteAppend:        counters.Mail,
	lockSiteExpunge:       counters.Mail,
	lockSiteResetFolder:   counters.Mail,
	lockSiteAlignUIDSpace: counters.Mail,
	lockSiteWriteFlags:    {counters.Email, counters.Mailbox},
}

// txMoves is what a committed transaction changed: flags alone leave threads
// and usage as they were.
func txMoves(ops []txOp) []counters.Type {
	var flags bool
	for _, op := range ops {
		switch op.kind {
		case opAppend, opExpunge:
			return counters.Mail
		case opUpdateFlags:
			flags = true
		}
	}
	if flags {
		return siteMoves[lockSiteWriteFlags]
	}
	return nil
}

// bump moves the account's counter. The write is already committed, so a
// failure here is logged, not returned: the client's write did succeed.
func (u *userIndex) bump(types ...counters.Type) {
	if u.counter == nil || len(types) == 0 {
		return
	}
	if err := u.counter.Bump(types...); err != nil {
		metricCounterBumpFailed.Inc()
		slog.Warn("fileindex: change counter not moved", "user", u.username, "err", err)
	}
}
