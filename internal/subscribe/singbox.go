package subscribe

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// parseSingBox accepts a sing-box config JSON and extracts the outbounds
// suitable for proxying (i.e. drops "direct" / "block" / "dns" / "selector"
// and friends; we keep only nodes that actually carry traffic).
//
// Unlike the clash/sip008/uri parsers, this one passes outbounds through
// as-is rather than rebuilding them field by field — sing-box's schema is
// already the internal one. That made it the only parser with no
// server/server_port validation until 2026-08-12: an entry missing either
// used to flow all the way through to the renderer, which emitted
// `server: <nil>` / `port: <nil>` (outboundToProxy in
// internal/mihomo/proxies.go reads those keys unconditionally).
//
// Validating here matters beyond a malformed proxy entry: the server field
// is the bootstrap key for fault-domain grouping. An empty server would
// bucket every such node under one "" key, i.e. report a set of unrelated
// nodes as sharing a fault domain — the failure mode is a silently wrong
// answer, not an error. Reject at the boundary instead, matching
// clashProxyToOutbound (internal/subscribe/clash.go:38).
func parseSingBox(body []byte) ([]Outbound, error) {
	var doc struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("singbox json: %w", err)
	}
	var out []Outbound
	for _, o := range doc.Outbounds {
		t, _ := o["type"].(string)
		if isInternalOutboundType(t) {
			continue
		}
		// Skip rather than error: one malformed entry in a subscription
		// must not discard the whole (otherwise-usable) subscription.
		// Refresh already drops a subscription wholesale on parse error
		// (internal/subscribe/parser.go), which would be a much larger
		// blast radius than losing one node.
		server, _ := o["server"].(string)
		if server == "" || getInt(o, "server_port") == 0 {
			tag, _ := o["tag"].(string)
			slog.Warn("subscribe: singbox outbound missing server/server_port, skipped",
				"tag", tag, "type", t)
			continue
		}
		out = append(out, Outbound(o))
	}
	return out, nil
}

func isInternalOutboundType(t string) bool {
	switch t {
	case "direct", "block", "dns", "selector", "urltest":
		return true
	}
	return false
}
