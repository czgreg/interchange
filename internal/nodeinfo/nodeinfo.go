// Package nodeinfo collects shell-out-y node + FeiLian + leap-services status
// in a background goroutine and exposes a cached snapshot so the API can
// answer /api/proxies/active without forking systemctl/nft per request.
//
// Cache TTL is intentionally short (5s) because operators poll this endpoint
// in tight loops while debugging, and "is leap-singbox up?" must reflect
// reality within a few seconds of an event.
package nodeinfo

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
)

// Snapshot is the structure returned to the API. Times use time.Time so
// json.Marshal emits RFC3339; bool fields are explicit so missing data is
// distinguishable from false.
type Snapshot struct {
	Node    NodeInfo    `json:"node"`
	FeiLian FeiLianInfo `json:"feilian"`
	Leap    LeapInfo    `json:"leap"`
}

type NodeInfo struct {
	Hostname      string      `json:"hostname"`
	Kernel        string      `json:"kernel"`
	OS            string      `json:"os"`
	UptimeSeconds int64       `json:"uptime_seconds"`
	Interfaces    []Interface `json:"interfaces"`
	ClientSubnet  string      `json:"client_subnet"`
	EgressIface   string      `json:"egress_iface"`
}

type Interface struct {
	Name string `json:"name"`
	IPv4 string `json:"ipv4,omitempty"`
}

type FeiLianInfo struct {
	Tun0Active       bool     `json:"tun0_active"`
	VPNActive        bool     `json:"vpn_active"`
	ProxyActive      bool     `json:"proxy_active"`
	SentryActive     bool     `json:"sentry_active"`
	NftChainsPresent []string `json:"nft_chains_present"`
}

type LeapInfo struct {
	GatewayVersion string            `json:"gateway_version"`
	// Engine is the active data-plane name: "mihomo" or "sing-box".
	Engine string `json:"engine"`
	// EngineVersion is the version string of the active data-plane binary
	// (mihomo's banner or sing-box's `version` output). Empty when the
	// binary is missing or doesn't respond. Replaces the older
	// `singbox_version` field which was misleading under engine=mihomo.
	EngineVersion string            `json:"engine_version"`
	Services      map[string]string `json:"services"`
}

// Reporter holds a goroutine-safe cached snapshot.
type Reporter struct {
	node     config.NodeConfig
	gatewayV string
	engine   string // active data-plane: "mihomo" or "sing-box"

	mu   sync.RWMutex
	snap Snapshot
}

func New(node config.NodeConfig, gatewayVersion, engine string) *Reporter {
	if engine == "" {
		engine = "sing-box"
	}
	return &Reporter{
		node:     node,
		gatewayV: gatewayVersion,
		engine:   engine,
	}
}

// Run starts a 5s ticker that refreshes the cached snapshot. Returns
// immediately; runs until ctx is cancelled. Safe to call multiple times
// (each call starts a new ticker).
func (r *Reporter) Run(ctx context.Context) {
	r.refresh(ctx)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.refresh(ctx)
		}
	}
}

// Snapshot returns the most recent cache. Empty fields just mean the
// background loop hasn't completed its first pass yet (or the relevant
// command failed — failures are silent on purpose, see the package comment).
func (r *Reporter) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snap
}

func (r *Reporter) refresh(ctx context.Context) {
	s := Snapshot{
		Node:    r.collectNode(),
		FeiLian: r.collectFeiLian(ctx),
		Leap:    r.collectLeap(ctx),
	}
	r.mu.Lock()
	r.snap = s
	r.mu.Unlock()
}

func (r *Reporter) collectNode() NodeInfo {
	host, _ := os.Hostname()
	return NodeInfo{
		Hostname:      host,
		Kernel:        unameR(),
		OS:            osPretty(),
		UptimeSeconds: readUptime(),
		Interfaces:    listInterfaces(),
		ClientSubnet:  r.node.ClientSubnet,
		EgressIface:   r.node.EgressIface,
	}
}

func (r *Reporter) collectFeiLian(ctx context.Context) FeiLianInfo {
	return FeiLianInfo{
		Tun0Active:       systemdActive(ctx, "feilian-tun@tun0.service"),
		VPNActive:        systemdActive(ctx, "feilian-vpn.service"),
		ProxyActive:      systemdActive(ctx, "feilian-vpn-proxy.service"),
		SentryActive:     systemdActive(ctx, "feilian-vpn-sentry.service"),
		NftChainsPresent: nftFeiLianChains(ctx),
	}
}

func (r *Reporter) collectLeap(ctx context.Context) LeapInfo {
	services := map[string]string{}
	// Both data-plane units always coexist on disk (install.sh ships both
	// units regardless of engine; only the engine in cfg.SingBox.Engine is
	// `enable`d). List both so operators can see at a glance which one is
	// the active engine — the other will be `inactive`.
	for _, u := range []string{
		"leap-gateway.service",
		"leap-mihomo.service",
		"leap-singbox.service",
		"leap-nft.service",
	} {
		services[strings.TrimSuffix(u, ".service")] = systemdState(ctx, u)
	}
	return LeapInfo{
		GatewayVersion: r.gatewayV,
		Engine:         r.engine,
		EngineVersion:  engineVersion(ctx, r.engine),
		Services:       services,
	}
}
