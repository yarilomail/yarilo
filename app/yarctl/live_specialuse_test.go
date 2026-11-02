package main

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/backendapi"
)

// specialuse list shows the configured defaults and the user's own overrides
// apart, so an operator can tell which one a client sees.
func TestSpecialUseListSeparatesDefaultsAndOverrides(t *testing.T) {
	b := startLiveBackendWith(t, func(o *backendapi.Options) {
		o.SpecialUseDefaults = map[string]string{"Sent": `\Sent`, "Trash": `\Trash`}
	})
	seedInbox(t, b, map[string]string{"1700000000.M1P1.host:2,S": liveMsg1})
	mustYarctl(t, "backend", "folder", "create", liveUser, "Projects")
	mustYarctl(t, "backend", "specialuse", "set", liveUser, "Projects", `\Archive`)

	var list struct {
		Overrides map[string]string `json:"overrides"`
		Defaults  map[string]string `json:"defaults"`
	}
	decode(t, mustYarctl(t, "backend", "specialuse", "list", liveUser), &list)
	if len(list.Overrides) != 1 || list.Overrides["Projects"] != `\Archive` {
		t.Errorf("overrides = %v, want Projects as \\Archive alone", list.Overrides)
	}
	if list.Defaults["Sent"] != `\Sent` || list.Defaults["Trash"] != `\Trash` {
		t.Errorf("defaults = %v, want the configured Sent and Trash", list.Defaults)
	}
	for _, args := range [][]string{
		{"backend", "specialuse", "list"},
		{"backend", "specialuse", "get", liveUser},
		{"backend", "specialuse", "set", liveUser, "Projects"},
		{"backend", "specialuse", "delete", liveUser},
	} {
		if _, err := yarctl(t, args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("yarctl %s = %v, want its usage", strings.Join(args, " "), err)
		}
	}
	if _, err := yarctl(t, "backend", "specialuse", "nosuch"); err == nil {
		t.Error("an unknown specialuse command was accepted")
	}
}
