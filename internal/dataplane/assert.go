// assert.go — periodic data-path assertion + self-repair.
//
// Why this exists (2026-08-12 outage, 1h43m of dead proxy traffic):
//
//   An unattended systemd upgrade (249.11-0ubuntu3.21 -> 3.22) restarted
//   systemd-networkd at 06:30:58. networkd defaults
//   ManageForeignRoutingPolicyRules=yes, which flushes every policy rule
//   not declared in a .network file EXCEPT rules with "proto kernel".
//   Our `ip rule add fwmark 0x44 lookup 101 pref 101` is proto unspec, so
//   it was deleted. Routing table 101 survived, because networkd's foreign
//   ROUTE cleanup is per-link and `lo` is unmanaged.
//
//   With the rule gone, TPROXY still marked packets and still set skb->sk,
//   but the post-TPROXY route lookup fell through to `main` -> default via
//   ens18 -> RTN_UNICAST -> ip_forward. Packets left the box carrying the
//   client's 10.8.12.x source and never reached mihomo's tproxy socket.
//   In the whole window only two destination classes still worked:
//   10.8.12.255 (RTN_BROADCAST from main) and 192.168.70.92 (RTN_LOCAL
//   from local) — 61 connections total, versus ~2700 per 10min before.
//
//   Nothing noticed for 103 minutes. /healthz was a hardcoded
//   {"ok": true}, and engine_ok only pinged mihomo's clash API, which
//   traverses no part of the TPROXY path. leap-nft.service is
//   Type=oneshot + RemainAfterExit=yes, so its ExecStart can never re-run
//   on its own — it is structurally unable to notice or repair the loss.
//
// The root cause is fixed on the node by
// /etc/systemd/networkd.conf.d/10-keep-leap-rules.conf
// (ManageForeignRoutingPolicyRules=no). This file is the defence in depth:
// it re-asserts every 30s, so any future remover — a networkd restart on a
// node missing that conf, a BindsTo=feilian-tun@tun0 stop propagating
// ExecStop=iproute.sh down, or an operator mistake — is repaired within one
// interval instead of lasting until someone notices.
//
// Design notes:
//   - Repair is MINIMAL and targeted. We deliberately do NOT shell out to
//     `iproute.sh up`: that flushes and re-adds the LEAP_TPROXY chain,
//     which would interrupt live traffic every time any single check
//     failed. Each check repairs only its own object.
//   - Repair still ALERTS. A silent self-heal would hide a recurring
//     remover and destroy the evidence that it is still happening.
//   - The end-to-end check is gated on clients actually being present, so
//     an idle night does not page anyone.
//   - exec of ip/iptables rather than netlink: the repo has no netlink
//     dependency, and iproute.sh already defines these objects through the
//     same CLI, so the two agree by construction.
package dataplane

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// TPROXY constants — must match deploy/node/iproute.sh. Divergence here
// means the asserter would "repair" objects into a shape iproute.sh then
// overwrites on the next leap-nft start, so keep them in lockstep.
const (
	tproxyMark  = "0x44"
	tproxyTable = "101"
)

// Check names, stable strings — they appear in /healthz output, in Lark
// alerts, and in notifications.log, so operators grep for them.
const (
	CheckIPRule         = "ip_rule_fwmark"
	CheckRouteLocal     = "route_table_local"
	CheckPreroutingJump = "prerouting_jump"
	CheckTProxyFlow     = "tproxy_flow"
)

// CheckResult is one assertion's outcome.
type CheckResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Detail is human-facing context: what was expected, what was found.
	Detail string `json:"detail,omitempty"`
	// Repaired is true when the check failed and repair succeeded. The
	// check is still reported as not-OK for this round so the alert fires
	// and /healthz reflects that something was broken.
	Repaired bool `json:"repaired,omitempty"`
	// Skipped marks a check that could not run meaningfully — currently
	// only the end-to-end flow check, when no clients are present.
	Skipped bool `json:"skipped,omitempty"`
}

// Snapshot is the asserter's latest full result, read by /healthz.
type Snapshot struct {
	At      time.Time     `json:"at"`
	Healthy bool          `json:"healthy"`
	Checks  []CheckResult `json:"checks"`
}

// Asserter periodically verifies the TPROXY data path and repairs it.
type Asserter struct {
	clientSubnet string
	tun0Iface    string
	tproxyPort   int
	clashAPI     config.ClashAPIConfig
	client       *http.Client

	// Emit reports a check transition to the operator. Non-nil in
	// production (wired to notify.Notifier); nil-safe for tests.
	Emit func(subject, body string, urgent bool)

	mu   sync.RWMutex
	snap Snapshot
	// lastAlerted tracks per-check alert state so a persistent failure
	// alerts on transition rather than every 30s. Recovery also alerts.
	lastAlerted map[string]bool
	// runner executes ip/iptables. Swappable in tests.
	runner func(ctx context.Context, name string, args ...string) (string, error)
}

// NewAsserter builds an Asserter for the given data-path parameters.
// tproxyPort <= 0 or an empty clientSubnet disables the asserter entirely
// (returns nil) — those mean gateway.yaml has no TPROXY data path to
// assert, and asserting nothing is better than repairing into port 0.
func NewAsserter(clientSubnet, tun0Iface string, tproxyPort int, clashAPI config.ClashAPIConfig) *Asserter {
	if clientSubnet == "" || tproxyPort <= 0 {
		return nil
	}
	if tun0Iface == "" {
		tun0Iface = "tun0"
	}
	return &Asserter{
		clientSubnet: clientSubnet,
		tun0Iface:    tun0Iface,
		tproxyPort:   tproxyPort,
		clashAPI:     clashAPI,
		client:       &http.Client{Timeout: 5 * time.Second},
		lastAlerted:  map[string]bool{},
		runner:       defaultRunner,
	}
}

func defaultRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// Snapshot returns the most recent assertion result. Zero value (At
// zero) means no round has completed yet.
func (a *Asserter) Snapshot() Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.snap
}

// Run blocks, asserting every interval until ctx is cancelled. The first
// round runs immediately so a broken path is caught at startup rather
// than one interval later.
func (a *Asserter) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	a.once(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.once(ctx)
		}
	}
}

func (a *Asserter) once(ctx context.Context) {
	checks := []CheckResult{
		a.checkIPRule(ctx),
		a.checkRouteLocal(ctx),
		a.checkPreroutingJump(ctx),
		a.checkTProxyFlow(ctx),
	}
	healthy := true
	for _, c := range checks {
		if !c.OK && !c.Skipped {
			healthy = false
		}
	}
	a.mu.Lock()
	a.snap = Snapshot{At: time.Now(), Healthy: healthy, Checks: checks}
	a.mu.Unlock()

	// Alert on transitions only: a check that has been failing for an hour
	// should not emit 120 identical messages.
	for _, c := range checks {
		if c.Skipped {
			continue
		}
		failing := !c.OK
		if failing && !a.lastAlerted[c.Name] {
			a.alert(c, true)
			a.lastAlerted[c.Name] = true
		} else if !failing && a.lastAlerted[c.Name] {
			a.alert(c, false)
			a.lastAlerted[c.Name] = false
		}
	}
}

func (a *Asserter) alert(c CheckResult, failing bool) {
	if a.Emit == nil {
		return
	}
	if failing {
		verb := "FAILED"
		if c.Repaired {
			verb = "FAILED and was auto-repaired"
		}
		a.Emit(
			fmt.Sprintf("data path check %s: %s", c.Name, verb),
			// The repaired case is still urgent: something removed a
			// data-path object, and knowing it recurs is the point.
			c.Detail,
			true,
		)
		return
	}
	a.Emit(fmt.Sprintf("data path check %s: recovered", c.Name), c.Detail, false)
}

// checkIPRule asserts `ip rule` contains fwmark <mark> lookup <table>.
// This single assertion is the entire 2026-08-12 outage, with no false
// positives: the rule is either present or proxy traffic is dead.
func (a *Asserter) checkIPRule(ctx context.Context) CheckResult {
	res := CheckResult{Name: CheckIPRule}
	out, err := a.runner(ctx, "ip", "rule", "show")
	if err != nil {
		res.Detail = fmt.Sprintf("ip rule show failed: %v: %s", err, strings.TrimSpace(out))
		return res
	}
	if rulePresent(out, tproxyMark, tproxyTable) {
		res.OK = true
		res.Detail = fmt.Sprintf("fwmark %s lookup %s present", tproxyMark, tproxyTable)
		return res
	}
	res.Detail = fmt.Sprintf("fwmark %s lookup %s MISSING — proxy traffic is being forwarded out instead of delivered to mihomo", tproxyMark, tproxyTable)
	// Repair: add just the rule. pref 101 matches iproute.sh.
	if rout, rerr := a.runner(ctx, "ip", "rule", "add", "fwmark", tproxyMark,
		"lookup", tproxyTable, "pref", "101"); rerr != nil {
		res.Detail += fmt.Sprintf("; repair failed: %v: %s", rerr, strings.TrimSpace(rout))
	} else {
		res.Repaired = true
		res.Detail += "; re-added"
	}
	return res
}

// rulePresent reports whether `ip rule show` output contains a rule
// matching both the fwmark and the lookup table. Matching is per-line and
// requires both tokens: `ip rule show` prints marks in hex without the
// 0x prefix on some kernels ("fwmark 0x44" vs "fwmark 44"), so accept
// either spelling.
func rulePresent(out, mark, table string) bool {
	bare := strings.TrimPrefix(mark, "0x")
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "lookup "+table) {
			continue
		}
		if strings.Contains(line, "fwmark "+mark) || strings.Contains(line, "fwmark "+bare) {
			return true
		}
	}
	return false
}

// checkRouteLocal asserts table <table> contains the local default route
// that makes the post-TPROXY lookup return RTN_LOCAL.
func (a *Asserter) checkRouteLocal(ctx context.Context) CheckResult {
	res := CheckResult{Name: CheckRouteLocal}
	out, err := a.runner(ctx, "ip", "route", "show", "table", tproxyTable)
	if err != nil {
		// An empty table exits non-zero on some iproute2 builds; treat as
		// missing rather than as an infrastructure error.
		res.Detail = fmt.Sprintf("table %s unreadable or empty: %v", tproxyTable, err)
	} else if strings.Contains(out, "local default") || strings.Contains(out, "local 0.0.0.0/0") {
		res.OK = true
		res.Detail = fmt.Sprintf("table %s has local default", tproxyTable)
		return res
	} else {
		res.Detail = fmt.Sprintf("table %s missing `local default dev lo`", tproxyTable)
	}
	if rout, rerr := a.runner(ctx, "ip", "route", "add", "local", "0.0.0.0/0",
		"dev", "lo", "table", tproxyTable); rerr != nil {
		res.Detail += fmt.Sprintf("; repair failed: %v: %s", rerr, strings.TrimSpace(rout))
	} else {
		res.Repaired = true
		res.Detail += "; re-added"
	}
	return res
}

// checkPreroutingJump asserts mangle PREROUTING still jumps client
// traffic into LEAP_TPROXY. Uses iptables-save rather than `nft list`:
// nft folds the TPROXY action into a comment, which previously caused a
// misread of intact rules as broken.
func (a *Asserter) checkPreroutingJump(ctx context.Context) CheckResult {
	res := CheckResult{Name: CheckPreroutingJump}
	out, err := a.runner(ctx, "iptables", "-t", "mangle", "-S", "PREROUTING")
	if err != nil {
		res.Detail = fmt.Sprintf("iptables -S PREROUTING failed: %v: %s", err, strings.TrimSpace(out))
		return res
	}
	// Match on the parts iproute.sh sets, not on exact argument order.
	found := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		l := sc.Text()
		if strings.Contains(l, "-j LEAP_TPROXY") &&
			strings.Contains(l, "-i "+a.tun0Iface) &&
			strings.Contains(l, "-s "+a.clientSubnet) {
			found = true
			break
		}
	}
	if found {
		res.OK = true
		res.Detail = fmt.Sprintf("PREROUTING -i %s -s %s -j LEAP_TPROXY present", a.tun0Iface, a.clientSubnet)
		return res
	}
	res.Detail = fmt.Sprintf("PREROUTING jump for -i %s -s %s MISSING", a.tun0Iface, a.clientSubnet)
	// Repair only the jump. If the LEAP_TPROXY chain itself is gone this
	// add fails, and that is the correct outcome: rebuilding the chain is
	// leap-nft's job (iproute.sh up) and doing it here would flush live
	// state on a 30s timer.
	if rout, rerr := a.runner(ctx, "iptables", "-t", "mangle", "-A", "PREROUTING",
		"-i", a.tun0Iface, "-s", a.clientSubnet, "-j", "LEAP_TPROXY"); rerr != nil {
		res.Detail += fmt.Sprintf("; repair failed (chain may be gone — run `systemctl restart leap-nft`): %v: %s",
			rerr, strings.TrimSpace(rout))
	} else {
		res.Repaired = true
		res.Detail += "; re-attached"
	}
	return res
}

// checkTProxyFlow is the end-to-end assertion: mihomo must be holding at
// least one connection whose metadata.type is TProxy. This is the only
// check that would have caught a failure mode where every object above
// is present but traffic still is not being intercepted.
//
// Gated on clients being present: with no client traffic there are
// legitimately zero TProxy connections, so a bare count would page every
// idle night. When the count is zero we report Skipped rather than
// guessing.
func (a *Asserter) checkTProxyFlow(ctx context.Context) CheckResult {
	res := CheckResult{Name: CheckTProxyFlow}
	url := fmt.Sprintf("http://%s/connections", a.clashAPI.ExternalController)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	if a.clashAPI.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+a.clashAPI.Secret)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		res.Detail = fmt.Sprintf("clash api /connections failed: %v", err)
		return res
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		res.Detail = fmt.Sprintf("clash api /connections status %d", resp.StatusCode)
		return res
	}
	var body struct {
		Connections []struct {
			Metadata struct {
				Type string `json:"type"`
				// SourceIP distinguishes real client flows from the
				// gateway's own loopback egress through mixed-port.
				SourceIP string `json:"sourceIP"`
			} `json:"metadata"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		res.Detail = fmt.Sprintf("decode /connections: %v", err)
		return res
	}
	tproxy, total := 0, len(body.Connections)
	for _, c := range body.Connections {
		if strings.EqualFold(c.Metadata.Type, "tproxy") {
			tproxy++
		}
	}
	switch {
	case tproxy > 0:
		res.OK = true
		res.Detail = fmt.Sprintf("%d/%d live connections are TProxy", tproxy, total)
	case total == 0:
		res.Skipped = true
		res.Detail = "no live connections at all — cannot distinguish idle clients from a broken path"
	default:
		// Connections exist but none is TProxy. Every object check above
		// may still pass in this state (e.g. mihomo's tproxy listener
		// down), which is exactly why this check exists.
		res.Detail = fmt.Sprintf("0 of %d live connections are TProxy — client interception is not working", total)
	}
	return res
}
