package protocol

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// clientConn is what the login process says about the mail client's
// connection on an AUTH line.
type clientConn struct {
	// secured is "tls", "secured" (trusted without TLS) or empty.
	secured  string
	clientID string
	// noPenalty is a client the login process trusts: no tarpit, no penalty.
	noPenalty bool
	// validCert is a client certificate the login process verified.
	validCert bool
	// fields are the remaining connection values, by their AUTH names.
	fields map[string]string
	// mechanism and cpid come from the AUTH line and the connection.
	mechanism string
	cpid      int
}

// connFields are the AUTH values kept as they are, for the variables the
// policy's templates may name.
var connFields = map[string]bool{
	"lport": true, "rport": true, "real_lip": true, "real_rip": true, "real_lport": true,
	"real_rport": true, "local_name": true, "ssl_ja3_hash": true, "ssl_client_cert_fp": true,
	"ssl_client_cert_pubkey_fp": true,
}

// parseField reads one AUTH field into c; false when the field is not one
// of its keys.
func (c *clientConn) parseField(f string) bool {
	key, value, hasValue := strings.Cut(f, "=")
	switch {
	case f == "secured":
		c.secured = "secured"
	case key == "secured" && hasValue:
		if value == "tls" {
			c.secured = "tls"
		} else {
			c.secured = "secured"
		}
	case key == "client_id" && hasValue:
		c.clientID = TabUnescape(value)
	case f == "no-penalty":
		c.noPenalty = true
	case f == "valid-client-cert":
		c.validCert = true
	case hasValue && connFields[key]:
		if c.fields == nil {
			c.fields = map[string]string{}
		}
		c.fields[key] = TabUnescape(value)
	default:
		return false
	}
	return true
}

// field is an AUTH value; a real_ one falls back to its plain counterpart, as
// the login sends real_ only when a proxy stands between.
func (c *clientConn) field(key, fallback string) string {
	if v, ok := c.fields[key]; ok {
		return v
	}
	return fallback
}

// policyOutcome is what the policy did to one login, for the audit log.
type policyOutcome struct {
	result  string // ok, delayed, refused; empty when no check ran
	penalty int
}

func (o policyOutcome) logAttrs() []any {
	if o.result == "" {
		return nil
	}
	return []any{"policy_result", o.result, "policy_penalty", o.penalty}
}

// policyCheck runs one check and applies its tarpit. Only the check before the
// exchange tarpits: after it the login has already paid its penalty.
func (s *Server) policyCheck(check string, req PolicyRequest, noPenalty bool, out *policyOutcome) PolicyDecision {
	var d PolicyDecision
	var err error
	if check == "before" {
		d, err = s.policy.CheckBefore(context.Background(), req)
	} else {
		d, err = s.policy.CheckAfter(context.Background(), req, true, false)
	}
	if err != nil {
		policyErrors.WithLabelValues(check).Inc()
		slog.Warn("auth: policy check error", "check", check, "err", err)
	}
	result := "ok"
	switch {
	case d.Reject:
		result = "refused"
	case d.TarpitSecs > 0:
		result = "delayed"
	}
	policyDecisions.WithLabelValues(check, result).Inc()
	if result == "refused" || out.result == "" || out.result == "ok" {
		out.result = result
	}
	if d.TarpitSecs > 0 && check == "before" && !noPenalty {
		out.penalty += d.TarpitSecs
		policyTarpitSeconds.Add(float64(d.TarpitSecs))
		s.sleep(time.Duration(d.TarpitSecs) * time.Second)
	}
	return d
}

// policyReport sends the report after a login, off the reply's path.
func (s *Server) policyReport(req PolicyRequest, success, policyReject bool) {
	if s.policy == nil || !s.policyMode.ReportAfter {
		return
	}
	go s.policy.ReportAfter(context.Background(), req, success, policyReject)
}

// refuseWith answers a policy refusal: its message as the reason, which the
// login process shows the client; without one, the mechanism's own refusal.
func refuseWith(msg string, reject func(net.Conn, string)) func(net.Conn, string) {
	if msg == "" {
		return reject
	}
	return func(conn net.Conn, id string) {
		fmt.Fprintf(conn, "FAIL\t%s\treason=%s\n", id, TabEscape(msg))
	}
}

var (
	policyDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "auth",
		Name:      "policy_decisions_total",
		Help:      "Policy server decisions by check (before, after) and result (ok, delayed, refused).",
	}, []string{"check", "result"})

	policyErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "auth",
		Name:      "policy_errors_total",
		Help:      "Policy checks that got no usable answer, by check; the fail-open or fail-closed decision was applied.",
	}, []string{"check"})

	policyTarpitSeconds = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo",
		Subsystem: "auth",
		Name:      "policy_tarpit_seconds_total",
		Help:      "Seconds of tarpit the policy server applied to logins.",
	})
)
