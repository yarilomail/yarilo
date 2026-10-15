package imap

import (
	"log/slog"

	imaplib "github.com/emersion/go-imap/v2"
	imapserver "github.com/emersion/go-imap/v2/imapserver"

	"github.com/yarilomail/yarilo/internal/storage/mailops"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// *session implements SessionQuota unconditionally so the library's cast
// succeeds; quotaExtensionEnabled() gates QUOTA, the mutation layer enforcement.
var _ imapserver.SessionQuota = (*session)(nil)

// quotaExtensionEnabled gates the IMAP QUOTA extension (GETQUOTA / capability).
func (s *session) quotaExtensionEnabled() bool {
	return s.srv.opts.IMAPQuota
}

// quotaName is the quota-root name surfaced to clients (default "User quota").
func (s *session) quotaName() string {
	if n := s.srv.opts.QuotaName; n != "" {
		return n
	}
	return quota.RootName
}

// quotaExceededMessage is the text of an over-quota rejection.
func (s *session) quotaExceededMessage() string {
	if m := s.srv.opts.QuotaExceededMessage; m != "" {
		return m
	}
	return "Quota exceeded"
}

// quotaPolicy returns the site-wide quota tunables.
func (s *session) quotaPolicy() quota.Policy {
	return s.srv.opts.QuotaPolicy
}

// mailopsConfig is this server's wiring of the mutation layer.
func (s *session) mailopsConfig() mailops.Config {
	o := s.srv.opts
	var rec *threads.Recorder
	if o.Threads != nil {
		rec = threads.NewRecorder(o.Threads)
	}
	return mailops.Config{
		Threads:      rec,
		Protocol:     "imap",
		Locker:       o.Locker,
		FTS:          o.FTS.Client,
		FTSAutoindex: o.FTS.Autoindex,
		FTSMaxRecent: o.FTS.MaxRecent,
		Quota: mailops.QuotaConfig{
			Engine:   o.QuotaEngine,
			Policy:   o.QuotaPolicy,
			MailSize: o.QuotaMailSize,
			Warner:   o.QuotaWarner,
			Clone:    o.QuotaClone,
		},
	}
}

// cloneFlushFinal hands the user back to the mirror; the last session of a
// user is where anything still pending is written, as the reference does in
// deinit_pre.
func (s *session) cloneFlushFinal() {
	if s.srv.opts.QuotaClone != nil && s.userInfo != nil {
		s.srv.opts.QuotaClone.Release(s.userInfo.Username)
	}
}

// GetQuotaRoot implements imapserver.SessionQuota.
func (s *session) GetQuotaRoot(mailbox string) (*imaplib.QuotaRootData, error) {
	if !s.quotaExtensionEnabled() {
		return &imaplib.QuotaRootData{Mailbox: mailbox}, nil
	}
	u, err := s.ops.Quota.Usage("getquota", true)
	if err != nil {
		slog.Warn("imap: quota get failed", "user", s.userInfo.Username, "err", err)
		return &imaplib.QuotaRootData{Mailbox: mailbox}, nil
	}
	limits := s.quotaPolicy().Scale(s.ops.Quota.Limits())
	// quota_hidden hides the root from every user; quota_ignore_unlimited hides
	// it only for users whose limits are all unlimited.
	if s.quotaPolicy().Hidden || (limits.Unlimited() && s.quotaPolicy().IgnoreUnlimited) {
		return &imaplib.QuotaRootData{Mailbox: mailbox}, nil
	}
	qd := buildQuotaData(u, limits, s.quotaName())
	return &imaplib.QuotaRootData{
		Mailbox: mailbox,
		Roots:   []string{s.quotaName()},
		Quotas:  []imaplib.QuotaData{qd},
	}, nil
}

// GetQuota implements imapserver.SessionQuota.
func (s *session) GetQuota(root string) (*imaplib.QuotaData, error) {
	if !s.quotaExtensionEnabled() {
		qd := imaplib.QuotaData{Name: root}
		return &qd, nil
	}
	u, err := s.ops.Quota.Usage("getquota", true)
	if err != nil {
		slog.Warn("imap: quota get failed", "user", s.userInfo.Username, "err", err)
		qd := imaplib.QuotaData{Name: root}
		return &qd, nil
	}
	limits := s.quotaPolicy().Scale(s.ops.Quota.Limits())
	if s.quotaPolicy().Hidden || (limits.Unlimited() && s.quotaPolicy().IgnoreUnlimited) {
		qd := imaplib.QuotaData{Name: root}
		return &qd, nil
	}
	qd := buildQuotaData(u, limits, s.quotaName())
	qd.Name = root
	return &qd, nil
}

// buildQuotaData constructs the IMAP QuotaData from current usage
// and resolved limits. STORAGE is in kibibytes per RFC 9208.
func buildQuotaData(u quota.Usage, lim quota.Limits, name string) imaplib.QuotaData {
	qd := imaplib.QuotaData{Name: name}
	storageUsageKiB := quota.StorageBytesToKiB(u.StorageBytes)
	storageLimitKiB := quota.StorageBytesToKiB(lim.StorageBytes)
	qd.Resources = append(qd.Resources, imaplib.QuotaResource{
		Type:  imaplib.QuotaResourceStorage,
		Usage: storageUsageKiB,
		Limit: storageLimitKiB,
	})
	if lim.Messages > 0 || u.Messages > 0 {
		qd.Resources = append(qd.Resources, imaplib.QuotaResource{
			Type:  imaplib.QuotaResourceMessage,
			Usage: uint64(u.Messages),
			Limit: uint64(lim.Messages),
		})
	}
	return qd
}

// quotaRefusal answers a save the quota refused with NO [OVERQUOTA]; the size
// refusal keeps its own text so a client can tell "too large" from "full".
func (s *session) quotaRefusal(refused *mailops.RefusedError) error {
	text := refused.Error()
	if refused.Reason == mailops.RefusedOverQuota {
		text = s.quotaExceededMessage()
	}
	return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCode("OVERQUOTA"), Text: text}
}
