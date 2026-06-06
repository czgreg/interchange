// inbounds.go — TUN block.
//
// mihomo's TUN inbound is a top-level `tun:` block (not a list of inbounds
// like sing-box). It also carries auto-route / auto-redir / dns-hijack
// flags that sing-box puts under route / dns. We keep auto-route DISABLED
// because leap-nft.service manages ip-rule + ip-route policy externally —
// we don't want mihomo competing with that.
//
// Interface name constraint: Linux IFNAMSIZ = 16, so the device name must
// be ≤ 15 chars. Validated at render time; fall back to "utun-leap" (the
// canonical name shared with sing-box) if a custom name is too long.

package mihomo

const (
	maxIfaceNameLen = 15
	defaultDevice   = "utun-leap"
)

func (r *Renderer) buildTUN() map[string]any {
	if !r.cfg.TUN.Enabled || r.node.Tun0GatewayIP == "" {
		return map[string]any{"enable": false}
	}
	device := r.cfg.TUN.InterfaceName
	if device == "" || len(device) > maxIfaceNameLen {
		device = defaultDevice
	}
	stack := r.cfg.TUN.Stack
	if stack == "" {
		stack = "system"
	}
	// gvisor (userspace) preserves the real client sourceIP — required for
	// per-terminal routing. system collapses sourceIP to the TUN device
	// address. mtu drops to 1500 under gvisor (9000 is a system-stack
	// optimization that gvisor's userspace path doesn't benefit from and
	// can mis-segment).
	mtu := 9000
	if stack == "gvisor" {
		mtu = 1500
	}
	return map[string]any{
		"enable":                 true,
		"device":                 device,
		"stack":                  stack,
		"auto-route":             false,
		"auto-redir":             false,
		"auto-detect-interface":  false,
		"dns-hijack":             []string{"any:53"},
		"strict-route":           false,
		"mtu":                    mtu,
		"include-interface":      []string{},
		"exclude-interface":      []string{},
	}
}
