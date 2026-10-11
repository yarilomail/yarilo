package config

import (
	"fmt"

	"github.com/knadh/koanf/v2"

	"github.com/yarilomail/yarilo/pkg/quota"
)

// intKey names one integer setting by its config path.
type intKey struct {
	path string
	v    *int
}

// offKeys are the settings where an explicit 0 turns the thing off; a key left
// out keeps Defaults().
func offKeys(cfg *Config) []intKey {
	d := &cfg.DirectorService
	return []intKey{
		{"threading.threading_cache_idle", &cfg.Threading.ThreadingCacheIdle},
		{"storage.storage_lock_stale_timeout", &cfg.Storage.LockStaleTimeout},
		{"storage.mail_cache_purge_delete_percentage", &cfg.Storage.MailCachePurgeDeletePercentage},
		{"storage.mail_cache_purge_continued_percentage", &cfg.Storage.MailCachePurgeContinuedPercentage},
		{"storage.mail_index_log_rotate_min_age", &cfg.Storage.MailIndexLogRotateMinAge},
		{"storage.max_concurrent_writes", &cfg.Storage.MaxConcurrentWrites},
		{"auth_service.auth_startup_wait", &cfg.AuthService.StartupWaitSeconds},
		{"locks_client.locks_client_startup_wait", &cfg.LocksClient.StartupWaitSeconds},
		{"auth_client.auth_client_pool_size", &cfg.AuthClient.PoolSize},
		{"auth_client.auth_client_pool_idle_timeout", &cfg.AuthClient.PoolIdleTimeoutSecs},
		{"login.transient_retries", &cfg.Login.TransientRetries},
		{"login.transient_relogin_cap", &cfg.Login.TransientReloginCap},
		{"login.lookup_hold_max", &cfg.Login.LookupHoldMax},
		{"login.lookup_hold_backoff_ms", &cfg.Login.LookupHoldBackoffMs},
		{"login.session_sync_interval", &cfg.Login.SessionSyncInterval},
		{"login.session_grace_period", &cfg.Login.SessionGracePeriod},
		{"internal_tls.session_cache_size", &cfg.InternalTLS.SessionCacheSize},
		{"internal_tls.session_cache_ttl", &cfg.InternalTLS.SessionCacheTTL},
		{"auth_service.shutdown.session_grace_period", &cfg.AuthService.Shutdown.SessionGracePeriod},
		{"warden_service.shutdown.session_grace_period", &cfg.WardenService.Shutdown.SessionGracePeriod},
		{"locks_service.shutdown.session_grace_period", &cfg.LocksService.Shutdown.SessionGracePeriod},
		{"director_service.shutdown.session_grace_period", &d.Shutdown.SessionGracePeriod},
		{"director_service.write_timeout", &d.WriteTimeout},
		{"director_service.anti_entropy_interval", &d.AntiEntropyInterval},
		{"director_service.tombstone_ttl", &d.TombstoneTTL},
		{"director_service.user_kick_delay", &d.UserKickDelay},
		{"director_service.max_parallel_kicks", &d.MaxParallelKicks},
		{"director_service.max_parallel_moves", &d.MaxParallelMoves},
		{"director_service.backend_expire", &d.BackendExpire},
		{"director_service.backend_unreachable_reporters", &d.BackendUnreachableReporters},
		{"director_service.backend_unreachable_window", &d.BackendUnreachableWindow},
		{"director_service.min_members", &d.MinMembers},
		{"director_service.seed_poll_interval", &d.SeedPollInterval},
		{"director_service.user_kill_confirm_grace", &d.UserKillConfirmGrace},
		{"director_service.director_domain_rebalance_percent", &d.DomainRebalancePercent},
		{"director_service.director_domain_rebalance_cooldown", &d.DomainRebalanceCooldown},
		{"general.limits.mail_max_userip_connections", &cfg.General.Limits.MaxUserIPConnections},
		{"protocol.imap.imap_idle_notify_interval", &cfg.Protocol.IMAP.IdleNotifyInterval},
		{"protocol.imap.imap_max_line_length", &cfg.Protocol.IMAP.MaxLineLength},
		{"protocol.jmap.jmap_max_calls_in_request", &cfg.Protocol.JMAP.MaxCallsInRequest},
		{"protocol.jmap.jmap_max_objects_in_get", &cfg.Protocol.JMAP.MaxObjectsInGet},
		{"protocol.jmap.jmap_max_objects_in_set", &cfg.Protocol.JMAP.MaxObjectsInSet},
		{"protocol.jmap.jmap_query_max_limit", &cfg.Protocol.JMAP.QueryMaxLimit},
		{"protocol.jmap.jmap_max_query_folders", &cfg.Protocol.JMAP.MaxQueryFolders},
		{"protocol.jmap.jmap_snippet_max_chars", &cfg.Protocol.JMAP.SnippetMaxChars},
		{"protocol.lmtp.lmtp_max_recipients", &cfg.Protocol.LMTP.MaxRecipients},
		{"protocol.submission.submission_max_recipients", &cfg.Protocol.Submission.MaxRecipients},
		{"protocol.managesieve.max_invalid_commands", &cfg.Protocol.ManageSieve.MaxInvalidCommands},
		{"sieve.sieve_max_script_size", &cfg.Sieve.MaxScriptSize},
		{"sieve.sieve_max_redirects", &cfg.Sieve.MaxRedirects},
		{"sieve.sieve_max_actions", &cfg.Sieve.MaxActions},
		{"sieve.sieve_duplicate_max_period", &cfg.Sieve.DuplicateMaxPeriod},
		{"acl.acl_cache_ttl", &cfg.ACL.CacheTTL},
		{"auth.auth_failure_delay", &cfg.Auth.FailureDelaySeconds},
		{"auth.internal_failure_delay_ms", &cfg.Auth.InternalFailureDelayMs},
		{"auth.cache.auth_cache_ttl", &cfg.Auth.Cache.TTLSeconds},
		{"auth.cache.auth_cache_negative_ttl", &cfg.Auth.Cache.NegativeTTLSeconds},
		{"quota.quota_clone_flush_delay", &cfg.Quota.CloneFlushDelay},
		{"quota_status.alias_max_hops", &cfg.QuotaStatus.AliasMaxHops},
		{"fts.fts_flatcurve_min_term_size", &cfg.FTS.FlatcurveMinTermSize},
		{"fts.fts_flatcurve_optimize_limit", &cfg.FTS.FlatcurveOptimizeLimit},
		{"fts.fts_flatcurve_rotate_count", &cfg.FTS.FlatcurveRotateCount},
		{"fts.fts_flatcurve_rotate_time", &cfg.FTS.FlatcurveRotateTimeMsecs},
		{"fts.fts_handle_idle_timeout", &cfg.FTS.HandleIdleTimeoutSecs},
		{"fts.fts_search_first_index_grace", &cfg.FTS.SearchFirstIndexGraceSecs},
		{"fts.fts_autoindex_max_recent_msgs", &cfg.FTS.AutoindexMaxRecentMsgs},
		{"fts.fts_detection_min_runes", &cfg.FTS.DetectionMinRunes},
		{"backend_register.vhosts", &cfg.BackendRegister.Vhosts},
	}
}

// requiredKeys are the settings that have no "off": an explicit 0 is refused,
// a key left out keeps Defaults().
func requiredKeys(cfg *Config) []intKey {
	d := &cfg.DirectorService
	return []intKey{
		{"general.haproxy.timeout", &cfg.General.HAProxy.Timeout},
		{"general.startup_dial_retries", &cfg.General.StartupDialRetries},
		{"sasl_login.haproxy_timeout", &cfg.SASLLogin.HAProxyTimeout},
		{"managesieve_login_service.haproxy_timeout", &cfg.ManageSieveLoginService.HAProxyTimeout},
		{"protocol.jmap.jmap_max_concurrent_requests", &cfg.Protocol.JMAP.MaxConcurrentRequests},
		{"protocol.lmtp.read_timeout", &cfg.Protocol.LMTP.ReadTimeout},
		{"protocol.lmtp.write_timeout", &cfg.Protocol.LMTP.WriteTimeout},
		{"protocol.lmtp.proxy.lmtp_proxy_timeout", &cfg.Protocol.LMTP.Proxy.ProxyTimeout},
		{"protocol.lmtp.rate_limit.rate_limit_per_recipient_burst", &cfg.Protocol.LMTP.RateLimit.PerRecipientBurst},
		{"protocol.lmtp.rate_limit.rate_limit_per_recipient_window_seconds", &cfg.Protocol.LMTP.RateLimit.PerRecipientWindowSeconds},
		{"protocol.submission.max_line_length", &cfg.Protocol.Submission.MaxLineLength},
		{"protocol.submission.relay.submission_relay_port", &cfg.Protocol.Submission.Relay.Port},
		{"protocol.submission.relay.submission_relay_connect_timeout", &cfg.Protocol.Submission.Relay.ConnectTimeout},
		{"protocol.submission.relay.submission_relay_command_timeout", &cfg.Protocol.Submission.Relay.CommandTimeout},
		{"protocol.managesieve.managesieve_max_line_length", &cfg.Protocol.ManageSieve.MaxLineLength},
		{"sieve.sieve_submission_timeout", &cfg.Sieve.SubmissionTimeout},
		{"sieve.sieve_pipe_exec_timeout", &cfg.Sieve.PipeExecTimeout},
		{"sieve.sieve_filter_exec_timeout", &cfg.Sieve.FilterExecTimeout},
		{"sieve.sieve_execute_exec_timeout", &cfg.Sieve.ExecuteExecTimeout},
		{"auth.auth_max_attempts", &cfg.Auth.MaxAttempts},
		{"auth.policy.timeout_ms", &cfg.Auth.Policy.TimeoutMs},
		{"auth.token.ttl_seconds", &cfg.Auth.Token.TTLSeconds},
		{"login.login_proxy_timeout", &cfg.Login.LoginProxyTimeout},
		{"director_service.ping_interval", &d.PingInterval},
		{"director_service.ping_timeout", &d.PingTimeout},
		{"director_service.seed_poll_idle_interval", &d.SeedPollIdleInterval},
		{"director_service.user_expire", &d.UserExpire},
		{"director_service.user_kill_timeout", &d.UserKillTimeout},
		{"director_service.director_domain_expire", &d.DomainExpire},
		{"director_service.director_domain_rebalance_interval", &d.DomainRebalanceInterval},
		{"director_service.flush_program_timeout", &d.FlushProgramTimeoutSeconds},
		{"dict_service.dict_max_conns", &cfg.DictService.DictMaxConns},
		{"locks_client.locks_client_wait_pool_size", &cfg.LocksClient.WaitPoolSize},
		{"warden_service.conns", &cfg.WardenService.Conns},
		{"warden_service.warden_service_event_queue_size", &cfg.WardenService.EventQueueSize},
		{"quota.quota_storage_percentage", &cfg.Quota.StoragePercentage},
		{"quota.quota_message_percentage", &cfg.Quota.MessagePercentage},
		{"quota.quota_warning_exec_timeout", &cfg.Quota.WarningExecTimeout},
		{"fts.fts_max_conns", &cfg.FTS.MaxConns},
		{"fts.fts_prefetch_depth", &cfg.FTS.PrefetchDepth},
		{"fts.fts_index_workers", &cfg.FTS.IndexWorkers},
		{"fts.fts_commit_limit", &cfg.FTS.CommitLimit},
		{"fts.fts_search_timeout", &cfg.FTS.SearchTimeoutSecs},
		{"fts.fts_decoder_timeout_secs", &cfg.FTS.DecoderTimeoutSecs},
		{"fts.fts_decoder_max_attempts", &cfg.FTS.DecoderMaxAttempts},
		{"fts.fts_flatcurve_commit_limit", &cfg.FTS.FlatcurveCommitLimit},
		{"fts.language_tokenizer_generic_token_maxlen", &cfg.FTS.LanguageTokenMaxLen},
		{"fts.language_tokenizer_address_token_maxlen", &cfg.FTS.LanguageAddressMaxLen},
		{"backend_register.register_interval", &cfg.BackendRegister.RegisterInterval},
		{"backend_register.readiness_touch_interval", &cfg.BackendRegister.ReadinessTouchInterval},
		{"backend_register.readiness_stale_after", &cfg.BackendRegister.ReadinessStaleAfter},
		{"telemetry.liveness_watchdog.liveness_watchdog_interval_seconds", &cfg.Telemetry.LivenessWatchdog.IntervalSeconds},
		{"telemetry.liveness_watchdog.liveness_watchdog_timeout_seconds", &cfg.Telemetry.LivenessWatchdog.TimeoutSeconds},
		{"telemetry.liveness_watchdog.liveness_watchdog_failure_threshold", &cfg.Telemetry.LivenessWatchdog.FailureThreshold},
	}
}

// checkNumbers refuses a negative value everywhere, and 0 where nothing can be
// turned off.
func checkNumbers(cfg *Config) error {
	for _, k := range offKeys(cfg) {
		if *k.v < 0 {
			return fmt.Errorf("config: %s: %d is negative; 0 turns it off", k.path, *k.v)
		}
	}
	for _, k := range requiredKeys(cfg) {
		if *k.v <= 0 {
			return fmt.Errorf("config: %s: %d; it must be positive (leave it out for the default)", k.path, *k.v)
		}
	}
	for path, v := range map[string]float64{"sieve.sieve_spamtest_max_value": cfg.Sieve.SpamMaxValue,
		"sieve.sieve_virustest_max_value": cfg.Sieve.VirusMaxValue} {
		if v <= 0 {
			return fmt.Errorf("config: %s: %g; it must be positive (leave it out for the default)", path, v)
		}
	}
	if w := cfg.Telemetry.LivenessWatchdog; w.TimeoutSeconds >= w.IntervalSeconds {
		return fmt.Errorf("config: telemetry.liveness_watchdog: timeout %d must stay below interval %d", w.TimeoutSeconds, w.IntervalSeconds)
	}
	for i, w := range cfg.Quota.Warnings {
		if w.Percentage <= 0 {
			return fmt.Errorf("config: quota.quota_warnings[%d].quota_warning_percentage: %d; it must be positive", i, w.Percentage)
		}
	}
	for i, e := range cfg.Auth.OAuth2 {
		if e.HTTPTimeoutMs <= 0 {
			return fmt.Errorf("config: auth.oauth2[%d].oauth2_http_timeout_ms: %d; it must be positive", i, e.HTTPTimeoutMs)
		}
		if e.TokenExpireGraceSeconds < 0 {
			return fmt.Errorf("config: auth.oauth2[%d].oauth2_token_expire_grace_seconds: %d is negative; 0 turns it off", i, e.TokenExpireGraceSeconds)
		}
	}
	for _, list := range []struct {
		path    string
		entries []PassdbEntry
	}{{"auth.passdb", cfg.Auth.Passdb}, {"auth.master_users.masterdb", cfg.Auth.MasterUsers.Masterdb}} {
		for i, e := range list.entries {
			for name, v := range map[string]*int{"max_open_conns": e.MaxOpenConns, "max_idle_conns": e.MaxIdleConns,
				"conn_max_lifetime": e.ConnMaxLifetime, "conn_max_idle_time": e.ConnMaxIdleTime} {
				if v != nil && *v < 0 {
					return fmt.Errorf("config: %s[%d].%s: %d is negative; 0 lifts the limit", list.path, i, name, *v)
				}
			}
		}
	}
	// No size means no rotation by size, which an mdbox has no use for.
	for path, raw := range map[string]string{"storage.mdbox_rotate_size": cfg.Storage.MdboxRotateSize,
		"fts.fts_detection_sample_bytes": cfg.FTS.DetectionSampleBytesRaw} {
		if quota.ParseSize(raw) <= 0 {
			return fmt.Errorf("config: %s: %q is not a size (leave it out for the default)", path, raw)
		}
	}
	return nil
}

// listEntryDefaults gives a list entry's numeric field its default when the
// entry leaves the key out; Defaults() cannot, since the entries come from the file.
func listEntryDefaults(k *koanf.Koanf, cfg *Config) {
	has := func(list string, i int, keys ...string) bool {
		entries, _ := k.Get(list).([]any)
		if i >= len(entries) {
			return false
		}
		m, _ := entries[i].(map[string]any)
		for _, key := range keys {
			if _, ok := m[key]; ok {
				return true
			}
		}
		return false
	}
	for i := range cfg.Auth.OAuth2 {
		e := &cfg.Auth.OAuth2[i]
		if !has("auth.oauth2", i, "oauth2_http_timeout_ms", "http_timeout_ms") {
			e.HTTPTimeoutMs = 5000
		}
		if !has("auth.oauth2", i, "oauth2_token_expire_grace_seconds", "token_expire_grace_seconds") {
			e.TokenExpireGraceSeconds = 60
		}
	}
	for i := range cfg.Quota.Warnings {
		if !has("quota.quota_warnings", i, "quota_warning_percentage") {
			cfg.Quota.Warnings[i].Percentage = 100
		}
	}
	for i := range cfg.DirectorService.MailServers {
		if !has("director_service.mail_servers", i, "vhosts") {
			cfg.DirectorService.MailServers[i].Vhosts = 100
		}
	}
}
