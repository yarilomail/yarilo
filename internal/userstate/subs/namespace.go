package subs

import (
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ForNamespace is where SUBSCRIBE would record a folder of namespace i: its own
// file under the relative name, else the personal file under the visible name.
func ForNamespace(namespaces []config.NamespaceConfig, i int, nsUI, personal *mailbox.UserInfo, owner string, locker locks.Locker) (*Store, string) {
	shapes := config.NamespaceShapes(namespaces)
	ns := namespaces[i]
	if ns.KeepsSubscriptions() {
		file := mailbox.SubsFileFor(shapes, i, ns.Prefix, separatorOf(ns))
		return New(mailbox.ControlRoot(nsUI), file, personal.Username, owner, locker), ""
	}
	file := "subscriptions"
	if p := mailbox.PrimaryPersonalIndex(shapes); p >= 0 {
		file = mailbox.SubsFileFor(shapes, p, namespaces[p].Prefix, separatorOf(namespaces[p]))
	}
	return New(mailbox.ControlRoot(personal), file, personal.Username, owner, locker), ns.Prefix
}

func separatorOf(ns config.NamespaceConfig) string {
	if ns.Separator == "" {
		return "."
	}
	return ns.Separator
}
