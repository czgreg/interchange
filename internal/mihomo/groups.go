// groups.go — proxy / proxy-groups assembly.
//
// The single biggest difference between mihomo and the sing-box renderer:
// no primary/backup split. ALL enabled subs' nodes pool into us-pool, a
// load-balance group with consistent-hashing strategy.
//
// Why: with sing-box urltest, exactly one node carries all traffic at a
// time — adding subscriptions only grows the candidate pool, not the
// load-bearing capacity. mihomo's load-balance + consistent-hashing makes
// every healthy node simultaneously active under different flows (each
// destination is hashed deterministically to a node).

package mihomo

import (
	"regexp"

	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

const (
	// usPool is the load-balance group of all NodePattern-matching nodes
	// across all enabled subscriptions. Receives all overseas traffic by
	// default. consistent-hashing keeps long-lived flows on a stable node.
	usPool = "us-pool"

	// pin is a manual selector — operator can PUT /proxies/pin to force
	// all "out"-routed traffic onto a single named node. Equivalent to
	// the watchdog's PUT /proxies/<sel> escape hatch verified in spike.
	pinSelector = "pin"

	// out is the top-level routing target: a selector with us-pool default.
	// Route rules send overseas traffic here; the selector decides which
	// underlying group/proxy actually carries it.
	outSelector = "out"

	// probeOutSelector is a select group over every candidate node, flipped
	// one node at a time by nodescorer to run site-specific probes through
	// the leap-probe listener.
	probeOutSelector = "probe-out"

	// probePort is the loopback HTTP listener nodescorer dials to probe
	// the node probe-out currently points at. 11081 (11080 is the leap-
	// internal proxy that follows `out`).
	probePort = 11081
)

// buildProxies emits the `proxies:` array — one entry per node-bearing
// outbound. Internal types (selector, urltest, direct, block, dns) are
// dropped — mihomo synthesizes its own DIRECT/REJECT pseudo-proxies.
func (r *Renderer) buildProxies(outbounds []subscribe.Outbound) []map[string]any {
	out := make([]map[string]any, 0, len(outbounds))
	for _, o := range outbounds {
		if p := outboundToProxy(o); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// buildProxyGroups emits the `proxy-groups:` array.
//
//	out (select)        : [us-pool, pin, DIRECT]
//	us-pool (load-bal)  : NodePattern-filtered subset of all parsed nodes,
//	                      strategy=consistent-hashing
//	pin (select)        : every NodePattern-matching node by name,
//	                      starts at the first as default, op-tunable via
//	                      PUT /proxies/pin
func (r *Renderer) buildProxyGroups(outbounds []subscribe.Outbound) []map[string]any {
	tags := nodeTags(outbounds)

	var pool []string
	if len(r.qualifiedOverride) > 0 {
		// NodeScorer hot-reload path: use the explicitly qualified set.
		// Still intersect with actually-present tags in case outbounds
		// changed between scoring and render (defensive).
		tagSet := make(map[string]bool, len(tags))
		for _, t := range tags {
			tagSet[t] = true
		}
		for _, t := range r.qualifiedOverride {
			if tagSet[t] {
				pool = append(pool, t)
			}
		}
	}
	if len(pool) == 0 {
		// Normal render path: filter by NodePattern.
		pool = filterByPattern(tags, r.cfg.URLTest.NodePattern)
		if len(pool) == 0 {
			pool = tags
		}
	}

	probeURL := r.cfg.URLTest.ProbeURL
	if probeURL == "" {
		probeURL = "http://www.gstatic.com/generate_204"
	}
	intervalSec := int(r.cfg.URLTest.Interval.Seconds())
	if intervalSec <= 0 {
		intervalSec = 60
	}

	groups := []map[string]any{
		{
			"name": outSelector,
			"type": "select",
			// The pool sits first so default routing uses it; pin is only
			// reached via explicit PUT /proxies/out {name: pin}.
			"proxies": []string{usPool, pinSelector, "DIRECT"},
		},
		{
			"name":     usPool,
			"type":     "load-balance",
			"strategy": "consistent-hashing",
			"proxies":  pool,
			"url":      probeURL,
			"interval": intervalSec,
			"lazy":     false,
		},
		{
			"name":    pinSelector,
			"type":    "select",
			"proxies": pool,
		},
	}

	// Named pools (openai-pool etc.) — one load-balance group each.
	// Members come from r.poolMembers[name] (us-pool ∩ passing that pool's
	// probes, set by nodescorer); absent → fall back to the full us-pool
	// set so the pool works before the first probe round completes.
	for _, p := range r.pools {
		members := pool
		if m, ok := r.poolMembers[p.Name]; ok {
			members = m
		}
		if len(members) == 0 {
			// No qualified members yet — fall back to full us-pool so the
			// rule routing to this pool doesn't dead-end at an empty group.
			members = pool
		}
		groups = append(groups, map[string]any{
			"name":     p.Name,
			"type":     "load-balance",
			"strategy": "consistent-hashing",
			"proxies":  members,
			"url":      probeURL,
			"interval": intervalSec,
			"lazy":     false,
		})
	}

	// probe-out: a select group over EVERY candidate node. nodescorer
	// flips it one node at a time and runs site probes through the
	// leap-probe listener (which is pinned to this group). Separate from
	// us-pool so probing never perturbs production routing.
	if r.probeListener {
		probeMembers := pool
		if len(tags) > 0 {
			probeMembers = tags // all node-bearing tags, not just pattern-matched
		}
		groups = append(groups, map[string]any{
			"name":    probeOutSelector,
			"type":    "select",
			"proxies": probeMembers,
		})
	}

	// Bootstrap edge case: empty pool. Emit a usable "out" that falls back
	// to DIRECT so mihomo loads. Drops us-pool/pin entirely.
	if len(pool) == 0 {
		return []map[string]any{
			{
				"name":    outSelector,
				"type":    "select",
				"proxies": []string{"DIRECT"},
			},
		}
	}
	return groups
}

// buildListeners emits the `listeners:` array. Currently just the leap-probe
// HTTP listener used by nodescorer's per-node site probes: a loopback HTTP
// proxy on probePort whose traffic is pinned (via `proxy:`) to the
// probe-out selector group, bypassing the normal rule table. nodescorer
// PUTs probe-out → <node>, then GETs the probe URLs through probePort to
// read real status codes + cf-mitigated headers.
func (r *Renderer) buildListeners() []map[string]any {
	return []map[string]any{
		{
			"name":   "leap-probe",
			"type":   "http",
			"listen": "127.0.0.1",
			"port":   probePort,
			"proxy":  probeOutSelector,
		},
	}
}

// nodeTags returns the tags of all node-bearing outbounds, preserving order.
func nodeTags(outbounds []subscribe.Outbound) []string {
	tags := make([]string, 0, len(outbounds))
	for _, o := range outbounds {
		if outboundToProxy(o) == nil {
			continue
		}
		if t := o.Tag(); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// filterByPattern returns tags matching the given regex pattern. Empty
// pattern means "all tags pass". Invalid regex falls back to all tags
// (we prefer over-broad to silently emptying the pool — same call as
// sing-box renderer's applyNodePattern).
func filterByPattern(tags []string, pattern string) []string {
	if pattern == "" {
		return tags
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return tags
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if re.MatchString(t) {
			out = append(out, t)
		}
	}
	return out
}
