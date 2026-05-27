package subscribe

import (
	"encoding/json"
	"fmt"
)

// SIP008 is a JSON array of Shadowsocks server entries, see
// https://shadowsocks.org/doc/sip008.html
type sip008Entry struct {
	Remarks    string `json:"remarks"`
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
	Method     string `json:"method"`
	Password   string `json:"password"`
	Plugin     string `json:"plugin"`
	PluginOpts string `json:"plugin_opts"`
}

func parseSIP008(body []byte) ([]Outbound, error) {
	// Some providers wrap the array in {"servers":[...]}.
	var wrapped struct {
		Servers []sip008Entry `json:"servers"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && len(wrapped.Servers) > 0 {
		return ssEntriesToOutbounds(wrapped.Servers), nil
	}
	var arr []sip008Entry
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, fmt.Errorf("sip008 json: %w", err)
	}
	return ssEntriesToOutbounds(arr), nil
}

func ssEntriesToOutbounds(arr []sip008Entry) []Outbound {
	var out []Outbound
	for i, e := range arr {
		if e.Server == "" || e.ServerPort == 0 {
			continue
		}
		tag := e.Remarks
		if tag == "" {
			tag = fmt.Sprintf("ss-%d", i)
		}
		o := Outbound{
			"type":        "shadowsocks",
			"tag":         tag,
			"server":      e.Server,
			"server_port": e.ServerPort,
			"method":      e.Method,
			"password":    e.Password,
		}
		if e.Plugin != "" {
			o["plugin"] = e.Plugin
			if e.PluginOpts != "" {
				o["plugin_opts"] = e.PluginOpts
			}
		}
		out = append(out, o)
	}
	return out
}
