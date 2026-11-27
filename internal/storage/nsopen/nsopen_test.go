package nsopen_test

import (
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/nsopen"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

func TestFixed(t *testing.T) {
	user := &mailbox.UserInfo{Username: "u1@example.com", Home: "/srv/mail/u1", SessionID: "s1"}
	for _, tc := range []struct {
		name     string
		ns       nsopen.Namespace
		expand   *mailbox.UserInfo
		kind     mailbox.StoreKind
		wantOK   bool
		wantHome string
		wantLock string
	}{
		{"public, one store for all", nsopen.Namespace{Type: "public", Prefix: "Public/", Separator: "/", Location: "maildir:/srv/public"},
			user, mailbox.SharedByAll, true, "/srv/public", "store:/srv/public"},
		{"expanded against the user", nsopen.Namespace{Type: "shared", Prefix: "Team/", Separator: "/", Location: "maildir:%h/team"},
			user, mailbox.UserStore, true, "/srv/mail/u1/team", ""},
		{"variables empty without expand", nsopen.Namespace{Type: "shared", Prefix: "Team/", Separator: "/", Location: "maildir:%h/team"},
			nil, mailbox.UserStore, true, "/team", ""},
		{"no location", nsopen.Namespace{Type: "public", Prefix: "Public/"}, user, mailbox.SharedByAll, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ui, ok, err := nsopen.Fixed(tc.ns, user, tc.expand, tc.kind)
			if err != nil || ok != tc.wantOK {
				t.Fatalf("Fixed = ok %v, err %v; want ok %v", ok, err, tc.wantOK)
			}
			if !ok {
				return
			}
			if ui.Home != tc.wantHome || ui.LockName != tc.wantLock || ui.Username != user.Username || ui.SessionID != user.SessionID {
				t.Errorf("Fixed = home %q lock %q user %q session %q", ui.Home, ui.LockName, ui.Username, ui.SessionID)
			}
		})
	}
}

func TestKind(t *testing.T) {
	for _, tc := range []struct {
		ns   nsopen.Namespace
		want mailbox.StoreKind
	}{
		{nsopen.Namespace{Type: "personal"}, mailbox.UserStore},
		{nsopen.Namespace{Type: "shared", Prefix: "user/%u/"}, mailbox.UserStore},
		{nsopen.Namespace{Type: "shared", Prefix: "Team/"}, mailbox.SharedByAll},
		{nsopen.Namespace{Type: "public", Prefix: "Public/"}, mailbox.SharedByAll},
	} {
		if got := nsopen.Kind(tc.ns); got != tc.want {
			t.Errorf("Kind(%+v) = %v, want %v", tc.ns, got, tc.want)
		}
	}
}

type backend struct{ name string }

func (b *backend) OpenUser(*mailbox.UserInfo) mailbox.UserMailbox { return nil }

func TestBackendsFor(t *testing.T) {
	global, sdbox, override := &backend{"global"}, &backend{"sdbox"}, &backend{"override"}
	b := nsopen.Backends{
		Mailbox: global,
		ByDriver: func(d string) mailbox.MailboxBackend {
			if d == "sdbox" {
				return sdbox
			}
			return nil
		},
		Overrides: map[string]mailbox.MailboxBackend{"Virtual/": override},
	}
	for _, tc := range []struct {
		name   string
		prefix string
		ui     *mailbox.UserInfo
		want   *backend
	}{
		{"override wins", "Virtual/", &mailbox.UserInfo{Driver: "sdbox"}, override},
		{"by driver", "Public/", &mailbox.UserInfo{Driver: "sdbox"}, sdbox},
		{"unknown driver", "Public/", &mailbox.UserInfo{Driver: "mdbox"}, global},
		{"no driver", "Public/", &mailbox.UserInfo{}, global},
		{"no identity", "Public/", nil, global},
	} {
		if got := b.For(tc.prefix, tc.ui); got != tc.want {
			t.Errorf("%s: For = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type store struct {
	mailbox.UserMailbox
	initErr       error
	inits, closes int
}

func (s *store) Init() error  { s.inits++; return s.initErr }
func (s *store) Close() error { s.closes++; return nil }

type storeBackend struct{ s *store }

func (b storeBackend) OpenUser(*mailbox.UserInfo) mailbox.UserMailbox { return b.s }

// A store whose Init fails is closed by the opener: no caller holds it (#2606).
func TestOpenClosesAStoreThatDoesNotInit(t *testing.T) {
	for _, tc := range []struct {
		name               string
		mode               nsopen.Mode
		initErr            error
		wantErr            bool
		wantInits, wantCls int
	}{
		{"init fails", nsopen.Init, errors.New("no room"), true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &store{initErr: tc.initErr}
			_, err := nsopen.Open(storeBackend{s}, nil, &mailbox.UserInfo{}, tc.mode, false)
			if (err != nil) != tc.wantErr || s.inits != tc.wantInits || s.closes != tc.wantCls {
				t.Errorf("Open = err %v, inits %d, closes %d; want err %v, inits %d, closes %d",
					err, s.inits, s.closes, tc.wantErr, tc.wantInits, tc.wantCls)
			}
		})
	}
}
