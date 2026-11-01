package imap

import (
	"reflect"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/v2/pkg/quota"
)

// A resource without a limit is left out of QUOTA: a limit of 0 tells a
// client the account may hold nothing and is full (#2455). Usage is never 0
// here, so a missing resource cannot pass for an empty one.
func TestQuotaListsOnlyLimitedResources(t *testing.T) {
	used := quota.Usage{StorageBytes: 23 * 1024, Messages: 55}
	storage := imaplib.QuotaResource{Type: imaplib.QuotaResourceStorage, Usage: 23, Limit: 100}
	messages := imaplib.QuotaResource{Type: imaplib.QuotaResourceMessage, Usage: 55, Limit: 1000}
	tests := []struct {
		name string
		lim  quota.Limits
		want []imaplib.QuotaResource
	}{
		{"both limits", quota.Limits{StorageBytes: 100 * 1024, Messages: 1000}, []imaplib.QuotaResource{storage, messages}},
		{"storage limit only", quota.Limits{StorageBytes: 100 * 1024}, []imaplib.QuotaResource{storage}},
		{"message limit only", quota.Limits{Messages: 1000}, []imaplib.QuotaResource{messages}},
		{"no limits", quota.Limits{}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			qd := buildQuotaData(used, tc.lim, "User quota")
			if qd.Name != "User quota" {
				t.Fatalf("root = %q, want User quota", qd.Name)
			}
			if !reflect.DeepEqual(qd.Resources, tc.want) {
				t.Fatalf("resources = %+v, want %+v", qd.Resources, tc.want)
			}
		})
	}
}
