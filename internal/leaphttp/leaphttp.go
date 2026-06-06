// Package leaphttp is the canonical HTTP-client factory for any leap-gateway
// outbound that talks to a host subject to mihomo's enhanced-mode fake-IP
// pollution (i.e. anything overseas — anything not on 127.0.0.0/8, the
// 10.8.x.x FeiLian client subnet, or other LAN ranges).
//
// # Why this package exists
//
// leap-gateway runs co-located with the mihomo data plane.
// Mihomo's fake-IP DNS rewrites every overseas A/AAAA answer to 198.18.x.x
// at the HOST RESOLVER level. A direct `net.Dial` from inside leap-gateway
// — running as the same user, sharing the same /etc/resolv.conf — to a
// host like raw.githubusercontent.com sees that fake IP and routes it
// through the host's main interface, where nothing is listening. Result:
// indefinite timeout. This was the root cause of the subscribe-fetcher
// outage memo'd as feilian-forwarding-node-quirks pitfall #6, and again
// of the whitelistexpand 6-domain bug caught 2026-06-06.
//
// # The pattern
//
// Dial THROUGH the loopback HTTP proxy mihomo exposes
// (singbox.LeapInternalProxyURL = http://127.0.0.1:11080). Mihomo's own
// DNS handles fake→real translation correctly because it can map the
// fake IP back to the original SNI/Host. Then the proxy egresses through
// us-pool to a real overseas peer.
//
// Two failure modes argue for "proxy with direct fallback", not "proxy
// only":
//
//  1. Bootstrap. mihomo isn't ready yet at process startup; the first
//     refresh-on-startup must succeed even before the data plane is up.
//     Falling back to direct dial during that window has a chance of
//     working (the fake-IP cache may not yet be populated for this host)
//     and is strictly better than failing.
//
//  2. Data-plane crash. mihomo restart cycle should not freeze every
//     leap-gateway outbound; a direct fallback at least lets refresh
//     attempts run while the operator investigates.
//
// # The rule
//
// Anything in this codebase that does `&http.Client{}` for an OVERSEAS
// host is a bug — it'll work intermittently (when fake-IP doesn't cover
// that host) and break catastrophically when it does. **Use NewClient
// always.** The only allowed exception is loopback-only HTTP (clash-api
// on 127.0.0.1:9090, leap-internal HTTP inbounds on 127.0.0.1:1108x):
// no fakeip risk there, no fallback complication.
//
// # Body rewinding
//
// The fallback retries the same `*http.Request` against a different
// transport. This is safe for GET/HEAD (idempotent, no body). For POST
// or other body-having requests, callers should set `req.GetBody` so
// the underlying RoundTripper can rewind — Go's standard library
// supports this when the body is built from `bytes.Buffer` /
// `strings.Reader` etc. via `http.NewRequest`. None of leap-gateway's
// current callers POST through this path; this note is a reminder for
// future use.
package leaphttp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// NewClient returns an *http.Client that dials through the leap-internal
// mihomo proxy at proxyURL, transparently falling back to a direct dial
// when the proxy errors at the transport layer (refused, dial timeout,
// TLS handshake failure, etc).
//
// HTTP-status errors (4xx/5xx) are NOT retried via fallback — the proxy
// successfully relayed bytes to the upstream and a different egress
// path won't change the upstream's mind. Caller checks `resp.StatusCode`
// after `Do()` returns nil error, as with any standard *http.Client.
//
// proxyURL = "" degrades to a single direct-dial client (no fallback).
// Used by tests, CLIs, and bootstrap rendering where no data plane is
// expected to exist.
//
// timeout caps the entire round-trip (connect + headers + body). 0 ⇒
// 30s default. Should be tuned per use case — subscription fetches use
// 30-60s, ruleset fetches use 60s, telemetry probes can be 5-10s.
//
// label is interpolated into fallback-warning slog lines (e.g. "subscribe",
// "whitelistexpand-domain", "whitelistexpand-srs", "rulesets") so journal
// scrapes can attribute failures back to the originating module without
// reading goroutine stacks.
func NewClient(proxyURL string, timeout time.Duration, label string) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if label == "" {
		label = "leaphttp"
	}
	direct := &http.Transport{}
	if proxyURL == "" {
		return &http.Client{Timeout: timeout, Transport: direct}
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		slog.Warn("leaphttp: invalid proxy url, degrading to direct",
			"label", label, "url", proxyURL, "err", err)
		return &http.Client{Timeout: timeout, Transport: direct}
	}
	via := &http.Transport{Proxy: http.ProxyURL(u)}
	return &http.Client{
		Timeout: timeout,
		Transport: &fallbackRT{
			primary:  via,
			fallback: direct,
			label:    label,
		},
	}
}

// fallbackRT is a RoundTripper that tries primary first; on transport-
// layer errors it retries on fallback. HTTP-status responses (any
// 1xx/2xx/3xx/4xx/5xx) are returned to the caller as-is — those are
// upstream answers, not transport failures.
type fallbackRT struct {
	primary  http.RoundTripper
	fallback http.RoundTripper
	label    string
}

func (r *fallbackRT) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.primary.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if !isTransportError(err) {
		return resp, err
	}
	slog.Warn("leaphttp: primary failed, retrying via direct",
		"label", r.label, "url", req.URL.String(), "err", err)
	// Body rewind is the caller's responsibility via req.GetBody — see
	// package doc. GET/HEAD have no body so this is moot for current
	// callers.
	return r.fallback.RoundTrip(req)
}

// isTransportError filters out errors that should NOT trigger fallback.
// Currently we treat ALL non-nil errors from a RoundTripper as transport
// errors (no response was produced) — that's the standard contract:
// http.RoundTripper returns either (resp, nil) or (nil, err); a non-
// nil err always means we never got bytes back from the upstream. The
// helper exists so future changes (e.g. respecting context cancellation
// without retry) have a place to land.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	// Don't retry context cancellation — the caller deliberately gave up.
	// Deadline-exceeded IS retried because the budget might cover the
	// fallback path and retry-once is cheaper than failing the call.
	if errors.Is(err, context.Canceled) {
		return false
	}
	return true
}
