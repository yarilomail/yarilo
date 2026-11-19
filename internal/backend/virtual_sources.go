package backend

import (
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/v2/internal/storage/nsopen"
	"github.com/yarilomail/yarilo/v2/internal/storage/virtualsrc"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// sharedSources is virtualsrc.Sources on this binary's backends (#2603).
func sharedSources(namespaces []config.NamespaceConfig, vd VirtualDeps, sc config.StorageConfig, locker locks.Locker, overrides map[string]mailbox.MailboxBackend) func(*mailbox.UserInfo, string) []virtual.Source {
	return virtualsrc.Sources(namespaces, virtualsrc.Deps{
		Backends: nsopen.Backends{
			Mailbox:   vd.Mailbox,
			ByDriver:  func(d string) mailbox.MailboxBackend { return buildMailboxByDriver(d, sc, locker) },
			Overrides: overrides,
		},
		Index: vd.Index, ACL: vd.ACL, SharedDict: vd.SharedDict, OwnerLookup: vd.OwnerLookup, Base: vd.Base, Locker: locker,
	})
}
