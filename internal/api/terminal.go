package api

import (
	"net/http"
	"strings"

	"github.com/leap-gateway/leap-gateway/internal/nodescorer"
)

// TerminalAssignment is the response shape for GET /api/pool/terminal?ip=X.
// Surfaces which egress nodes a terminal IP currently routes through, with
// each node's probe / passive health so operators can diagnose user
// complaints without correlating multiple endpoints.
type TerminalAssignment struct {
	IP         string                 `json:"ip"`
	Subnet     string                 `json:"subnet"`               // configured client_subnet
	InSubnet   bool                   `json:"in_subnet"`            // false → terminal is outside enumerated range, falls through to MATCH,us-pool
	Primary    *terminalNodeView      `json:"primary,omitempty"`    // current default egress for this terminal
	Secondary  *terminalNodeView      `json:"secondary,omitempty"`  // mihomo fallback target when primary is alive=false
	ActiveNode string                 `json:"active_node,omitempty"` // which one is actually carrying traffic right now ("primary" | "secondary" | "")
	GroupName  string                 `json:"group_name,omitempty"` // e.g. fb-10.8.13.42 — present when InSubnet
	PoolMode   string                 `json:"pool_mode"`            // "manual" | "auto" | other
	Hint       string                 `json:"hint,omitempty"`       // operator-facing diagnostic note
}

// terminalNodeView is the per-node detail surfaced for primary / secondary
// of a terminal assignment. Embeds the full NodeHealth (RTT stats, probes,
// passive stats) so a single API call gives the operator everything needed
// to reason about a user's experience.
type terminalNodeView struct {
	Name   string                 `json:"name"`
	Alive  bool                   `json:"alive"`
	Health *nodescorer.NodeHealth `json:"health,omitempty"` // omitted when scorer hasn't seen this node yet
}

// handlePoolTerminal answers GET /api/pool/terminal?ip=10.8.13.42
//
// Resolves: terminal IP → fb-<ip> group → [primary, secondary] HRW
// assignment → each node's NodeHealth + alive bit. Decides "active_node"
// by inspecting primary's alive bit (mihomo's fallback semantics use the
// first alive member).
//
// Returns 400 when ip is missing/malformed, 503 when the scorer isn't
// running (engine != mihomo), 200 with InSubnet=false when the IP is
// outside client_subnet (still useful for the operator to see the
// client_subnet config + know that they're querying outside it).
func (s *Server) handlePoolTerminal(w http.ResponseWriter, r *http.Request) {
	ip := strings.TrimSpace(r.URL.Query().Get("ip"))
	if ip == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "missing ?ip= query parameter",
		})
		return
	}
	if s.deps.NodeScorer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "scorer not active (requires engine=mihomo + node_qualify.enabled=true)",
		})
		return
	}

	outbounds := s.deps.Subscribe.AllOutbounds()
	assigned, inSubnet := s.deps.Renderer.AssignmentForIP(ip, outbounds)

	resp := TerminalAssignment{
		IP:       ip,
		Subnet:   s.deps.Cfg.Node.ClientSubnet,
		InSubnet: inSubnet,
		PoolMode: s.deps.Cfg.NodeQualify.PoolMode,
	}
	if !inSubnet {
		resp.Hint = "ip is outside client_subnet — traffic from this source falls through to MATCH,us-pool, not a per-terminal fb group"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if len(assigned) == 0 {
		resp.Hint = "routing pool is empty — no fb-<ip> group rendered"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.GroupName = "fb-" + ip

	snap := s.deps.NodeScorer.GetSnapshot()
	byName := make(map[string]*nodescorer.NodeHealth, len(snap.Nodes))
	for i := range snap.Nodes {
		byName[snap.Nodes[i].Name] = &snap.Nodes[i]
	}

	makeView := func(name string) *terminalNodeView {
		v := &terminalNodeView{Name: name}
		if h, ok := byName[name]; ok {
			v.Alive = h.Alive
			v.Health = h
		}
		return v
	}
	resp.Primary = makeView(assigned[0])
	if len(assigned) > 1 {
		resp.Secondary = makeView(assigned[1])
	}

	// active_node is what mihomo's fallback would currently pick: primary
	// if alive=true, else secondary. We surface this as a hint — for an
	// authoritative answer the operator can read mihomo's /connections
	// to see which proxy this terminal's live flows are actually using.
	switch {
	case resp.Primary != nil && resp.Primary.Alive:
		resp.ActiveNode = "primary"
	case resp.Secondary != nil && resp.Secondary.Alive:
		resp.ActiveNode = "secondary"
		resp.Hint = "primary is alive=false; mihomo fallback should be routing through secondary"
	default:
		resp.ActiveNode = ""
		resp.Hint = "neither primary nor secondary is alive — terminal traffic is currently failing; check emergency_promote_chain or pool_members"
	}
	writeJSON(w, http.StatusOK, resp)
}
