// Package virtualsrc opens, for a virtual mailbox, the stores of the other
// namespaces its patterns may reach, as a session opens them (#2603).
package virtualsrc

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/internal/storage/nsopen"
	"github.com/yarilomail/yarilo/v2/internal/userstate/acl"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/dict"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// Deps is what the stores are opened with: the backends and index a session
// uses, its ACL settings, and for an owner-templated namespace the owners'
// registry, userdb lookup and the deployment's storage-name form.
type Deps struct {
	Backends    nsopen.Backends
	Index       mailbox.IndexBackend
	ACL         config.ACLConfig
	SharedDict  dict.Dict
	OwnerLookup func(context.Context, string) (*mailbox.UserInfo, error)
	Base        *mailbox.UserInfo
	Locker      locks.Locker
}

// virtualNamespace is a namespace a virtual mailbox may draw from besides the
// personal one: a shared one, the only other type a session opens.
func virtualNamespace(ns config.NamespaceConfig) bool {
	return strings.EqualFold(strings.TrimSpace(ns.Type), "shared")
}

// Declared is every namespace but the personal ones, prefixes in "/"
// form, as a virtual pattern resolves to them (#2603).
func Declared(namespaces []config.NamespaceConfig) []virtual.Declared {
	var out []virtual.Declared
	for _, ns := range namespaces {
		if strings.EqualFold(strings.TrimSpace(ns.Type), "personal") || ns.Prefix == "" {
			continue
		}
		loc, ok, _ := mailbox.ParseLocation(ns.Location, nil)
		out = append(out, virtual.Declared{
			Prefix:         virtual.SameSeparator(ns.Prefix, sepOf(ns)),
			OwnerTemplated: mailbox.PrefixIsOwnerTemplated(ns.Prefix),
			HasStore:       virtualNamespace(ns) && ok && !strings.EqualFold(loc.Driver, "virtual"),
		})
	}
	return out
}

// Sources opens, for one user, the stores of the shared and public
// namespaces as a session opens them, read only: through nsopen, with the
// session's ACL (#2603). An owner-templated namespace gives a store per owner
// who granted the user something.
func Sources(namespaces []config.NamespaceConfig, vd Deps) func(*mailbox.UserInfo) []virtual.Source {
	if vd.Backends.Mailbox == nil || vd.Index == nil {
		return nil
	}
	var global *acl.Global
	if vd.ACL.Enabled {
		g, err := acl.NewGlobal(vd.ACL.Global)
		if err != nil {
			slog.Error("virtualsrc: virtual mailboxes draw on no shared namespace: global ACL", "err", err)
			return nil
		}
		global = g
	}
	backends, locker := vd.Backends, vd.Locker
	return func(personal *mailbox.UserInfo) []virtual.Source {
		var out []virtual.Source
		for _, ns := range namespaces {
			if !virtualNamespace(ns) || ns.Location == "" {
				continue
			}
			nsd := nsopen.Namespace{Type: ns.Type, Prefix: ns.Prefix, Separator: ns.Separator, Location: ns.Location}
			declared := virtual.SameSeparator(ns.Prefix, sepOf(ns))
			for _, st := range storesOf(nsd, vd, personal) {
				if strings.EqualFold(st.ui.Driver, "virtual") {
					continue
				}
				o, err := nsopen.Open(backends.For(ns.Prefix, st.ui), vd.Index, st.ui, nsopen.Deferred, false, mailboxbase.ReadOnly())
				if err != nil {
					slog.Warn("virtualsrc: a virtual mailbox skips a namespace", "ns", ns.Prefix, "err", err)
					continue
				}
				out = append(out, virtual.Source{
					Namespace: declared, Prefix: st.prefix, Sep: sepOf(ns), Box: o.Box,
					Rights: rightsOn(vd.ACL, global, ns, st, personal, locker),
				})
			}
		}
		return out
	}
}

// store is one store of a namespace: the identity it opens as, its IMAP
// prefix, and its owner when it is one person's.
type store struct {
	ui     *mailbox.UserInfo
	prefix string
	owner  string
}

func storesOf(nsd nsopen.Namespace, vd Deps, personal *mailbox.UserInfo) []store {
	if !mailbox.PrefixIsOwnerTemplated(nsd.Prefix) {
		ui, ok, err := nsopen.Fixed(nsd, personal, personal, nsopen.Kind(nsd))
		if err != nil || !ok {
			if err != nil {
				slog.Warn("virtualsrc: a virtual mailbox skips a namespace", "ns", nsd.Prefix, "err", err)
			}
			return nil
		}
		return []store{{ui: ui, prefix: nsd.Prefix}}
	}
	if vd.SharedDict == nil || vd.OwnerLookup == nil {
		return nil
	}
	user, groups := personal.ACLIdentity()
	owners, err := acl.OwnersFor(context.Background(), vd.SharedDict, user, groups)
	if err != nil {
		slog.Warn("virtualsrc: a virtual mailbox reads no owner-templated namespace", "ns", nsd.Prefix, "err", err)
		return nil
	}
	before, after, _ := strings.Cut(nsd.Prefix, mailbox.OwnerVar)
	var out []store
	for _, owner := range owners {
		found, err := vd.OwnerLookup(context.Background(), owner)
		if err != nil || found == nil {
			continue
		}
		found.SessionID = personal.SessionID
		ui, err := nsopen.Owner(nsd, found, vd.Base)
		if err != nil {
			continue
		}
		out = append(out, store{ui: ui, prefix: before + owner + after, owner: owner})
	}
	return out
}

// rightsOn is the user's rights on a folder of st, as a session resolves them.
func rightsOn(cfg config.ACLConfig, global *acl.Global, ns config.NamespaceConfig, st store, personal *mailbox.UserInfo, locker locks.Locker) func(string) (mailbox.Rights, error) {
	if !cfg.Enabled || ns.IgnoreACL {
		return func(string) (mailbox.Rights, error) { return mailbox.FullRights, nil }
	}
	typ := strings.ToLower(strings.TrimSpace(ns.Type))
	store := nsopen.ACL(st.ui.Home, st.ui, st.ui.Username, locks.Owner(personal.Username, personal.SessionID), acl.Policy{
		DefaultsFromInbox: cfg.DefaultsFromInbox && typ == "shared",
		GlobalsOnly:       cfg.GlobalsOnly,
		CacheTTL:          time.Duration(cfg.CacheTTL) * time.Second,
		Global:            global,
	}, locker)
	user, groups := personal.ACLIdentity()
	isOwner := st.owner != "" && st.owner == personal.Username
	sep := sepOf(ns)[0]
	return func(rel string) (mailbox.Rights, error) {
		return store.EffectiveFor(rel, user, groups, isOwner, sep)
	}
}

// sepOf is the namespace's separator as an IMAP session reads it: "." when
// none is configured.
func sepOf(ns config.NamespaceConfig) string {
	if ns.Separator == "" {
		return "."
	}
	return ns.Separator
}
