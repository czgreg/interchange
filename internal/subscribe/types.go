package subscribe

// Outbound is a single sing-box outbound config (ready to be embedded into
// the rendered sing-box config.json). Parsers normalize all subscription
// formats into this shape so the renderer is format-agnostic.
type Outbound map[string]any

// SubscriptionResult is the parsed result of one subscription URL.
type SubscriptionResult struct {
	Name      string
	Format    string
	Outbounds []Outbound
}

// Tag returns the sing-box tag of an outbound, or "" if missing.
func (o Outbound) Tag() string {
	if v, ok := o["tag"].(string); ok {
		return v
	}
	return ""
}

// Type returns the sing-box outbound type ("vmess" / "shadowsocks" / ...).
func (o Outbound) Type() string {
	if v, ok := o["type"].(string); ok {
		return v
	}
	return ""
}
