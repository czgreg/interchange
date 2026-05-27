package subscribe

import (
	"encoding/json"
	"fmt"
)

// parseSingBox accepts a sing-box config JSON and extracts the outbounds
// suitable for proxying (i.e. drops "direct" / "block" / "dns" / "selector"
// and friends; we keep only nodes that actually carry traffic).
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
