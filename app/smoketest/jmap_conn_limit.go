package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// checkJMAPConnLimit holds one connection more than the deployment's JMAP
// limit and wants the refusal as 429 with Retry-After, not 503 (#2472).
func checkJMAPConnLimit() error {
	url := "https://" + net.JoinHostPort(jmapHost(), *flagJMAPPort) + "/.well-known/jmap"
	return jmapConnLimitAt(url, publicTLS(jmapHost()), *flagJMAPUser, *flagJMAPPass, *flagJMAPConnLimit, *flagTimeout)
}

// jmapConnLimitAt opens connections one by one, each kept open, until one is
// refused. Connections the client already holds count too, so the refusal
// may come early but never later than limit+1. After the held ones close, a
// new connection must pass again, or a refusal outlives its cause.
func jmapConnLimitAt(url string, tlsConf *tls.Config, user, pass string, limit int, timeout time.Duration) error {
	var held []*http.Transport
	defer func() {
		for _, tr := range held {
			tr.CloseIdleConnections()
		}
	}()
	refusedAt := 0
	for i := 1; i <= limit+1; i++ {
		tr := &http.Transport{TLSClientConfig: tlsConf, MaxIdleConnsPerHost: 1}
		held = append(held, tr)
		status, retryAfter, err := jmapConnGet(tr, url, user, pass, timeout)
		if err != nil {
			return fmt.Errorf("connection %d: %w", i, err)
		}
		if status == http.StatusOK {
			continue
		}
		if status != http.StatusTooManyRequests {
			return fmt.Errorf("connection %d of limit %d answered HTTP %d, want 429", i, limit, status)
		}
		if s, err := strconv.Atoi(retryAfter); err != nil || s < 0 {
			return fmt.Errorf("429 at connection %d carries Retry-After %q, want seconds", i, retryAfter)
		}
		refusedAt = i
		break
	}
	if refusedAt == 0 {
		return fmt.Errorf("%d connections passed a limit of %d", limit+1, limit)
	}
	for _, tr := range held {
		tr.CloseIdleConnections()
	}
	held = held[:0]
	deadline := time.Now().Add(timeout)
	for {
		tr := &http.Transport{TLSClientConfig: tlsConf, DisableKeepAlives: true}
		status, _, err := jmapConnGet(tr, url, user, pass, timeout)
		if err == nil && status == http.StatusOK {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("after closing the held connections: %w", err)
			}
			return fmt.Errorf("after closing the held connections a new one still answers HTTP %d", status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// jmapConnGet authenticates once over tr and drains the body, so the
// connection goes back to tr's pool and stays open.
func jmapConnGet(tr *http.Transport, url, user, pass string, timeout time.Duration) (status int, retryAfter string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.SetBasicAuth(user, pass)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("Retry-After"), nil
}
