package mailops

import (
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/quotawarn"
	"github.com/yarilomail/yarilo/v2/pkg/quota"
)

// A delivery that takes the account past a warning's share is told so at the
// check, before the message is stored; one that stays under is not.
func TestADeliveryCrossingAWarningFiresIt(t *testing.T) {
	cfg, box, info := fullAccount(t, "*:messages=4")
	cfg.Quota.Policy.Warnings = []quota.Warning{{Name: "half", Resource: "message", Threshold: "over", Percentage: 50}}
	cfg.Quota.Warner = quotawarn.New(t.TempDir(), 5)
	log := logged(t)
	// One message held of four: the next is the second, the 50% line.
	if err := NewDelivery(cfg, box, info).Quota.CheckSave("INBOX", 10); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "quota warning crossed") {
		t.Error("the delivery that reached half the limit fired nothing")
	}
	log.Reset()
	info.QuotaRules = []string{"*:messages=100"}
	if err := NewDelivery(cfg, box, info).Quota.CheckSave("INBOX", 10); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), "quota warning crossed") {
		t.Error("a delivery far under the line fired the warning")
	}
}

// The userdb over-flag is trusted only right after login: a session asked
// later counts the account but leaves the flag alone.
func TestALateOverStatusCheckLeavesTheFlag(t *testing.T) {
	o, _ := overStatusUser(t, "", 2, true)
	o.Quota.loginAt = time.Now().Add(-time.Minute)
	log := logged(t)
	if _, err := o.Quota.Usage("test", false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(log.String(), "over-status") {
		t.Error("a check a minute after login ran")
	}
}
