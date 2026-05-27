package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	API           APIConfig           `yaml:"api"`
	Subscribe     SubscribeConfig     `yaml:"subscribe"`
	Subscriptions []SubscriptionEntry `yaml:"subscriptions"`
	Node          NodeConfig          `yaml:"node"`
	SingBox       SingBoxConfig       `yaml:"singbox"`
}

type APIConfig struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
}

type SubscribeConfig struct {
	RefreshInterval  time.Duration `yaml:"refresh_interval"`
	RefreshOnStartup bool          `yaml:"refresh_on_startup"`
	HTTPTimeout      time.Duration `yaml:"http_timeout"`
	UserAgent        string        `yaml:"user_agent"`
}

type SubscriptionEntry struct {
	Name    string `yaml:"name"`
	URL     string `yaml:"url"`
	Format  string `yaml:"format"` // auto | clash | singbox | uri | sip008
	Enabled bool   `yaml:"enabled"`
}

// NodeConfig describes the FeiLian forwarding node that leap is sidecared on.
// All three fields are assigned by FeiLian SaaS when the node is added to the
// control plane. They cannot be guessed — the deploy operator fills them in.
type NodeConfig struct {
	// ClientSubnet is the CIDR of the client pool (e.g. "10.8.11.0/24").
	ClientSubnet string `yaml:"client_subnet"`
	// Tun0GatewayIP is the IP address sing-box DNS server binds to
	// (e.g. "10.8.11.1"). It must be the gateway of tun0 — the address that
	// the FeiLian client sees as the gateway of its tunnel.
	Tun0GatewayIP string `yaml:"tun0_gateway_ip"`
	// EgressIface is the interface that the node uses to reach the public
	// internet (e.g. "ens18"). Used for sing-box auto_detect_interface fallback.
	EgressIface string `yaml:"egress_iface"`
}

type SingBoxConfig struct {
	ConfigPath string `yaml:"config_path"`
	LogLevel   string `yaml:"log_level"`

	ClashAPI ClashAPIConfig `yaml:"clash_api"`

	// TUN drives the TUN inbound that catches forwarded traffic redirected to
	// us via fwmark policy routing.
	TUN TUNConfig `yaml:"tun"`

	// DNS drives the sing-box DNS server upstreams.
	DNS DNSConfig `yaml:"dns"`

	// Route drives the sing-box route.rule_set URLs.
	Route RouteConfig `yaml:"route"`

	// URLTest tunes the auto-selected outbound's behavior + the watchdog that
	// guards it.
	URLTest URLTestConfig `yaml:"urltest"`

	// Legacy ad-hoc inbounds, retained so cmd/selftest renders something
	// sing-box check can validate without a full deployment. If left empty,
	// these inbounds are simply omitted.
	SOCKSListen string `yaml:"socks_listen"`
	HTTPListen  string `yaml:"http_listen"`
}

type ClashAPIConfig struct {
	ExternalController string `yaml:"external_controller"`
	Secret             string `yaml:"secret"`
}

type TUNConfig struct {
	Enabled       bool   `yaml:"enabled"`
	InterfaceName string `yaml:"interface_name"`
	Address       string `yaml:"address"` // e.g. "172.19.0.1/30"
	FWMark        int    `yaml:"fwmark"`
	RoutingTable  int    `yaml:"routing_table"`
}

type DNSConfig struct {
	// Listen overrides the default <Tun0GatewayIP>:53. Useful in lab.
	Listen string `yaml:"listen"`
	// FakeIPRange is the IPv4 fake-IP pool. Default 198.18.0.0/15.
	FakeIPRange string `yaml:"fakeip_range"`
	// CNDoH is the upstream pool for CN-side resolution.
	CNDoH []string `yaml:"cn_doh"`
	// ProxyDoH is the upstream pool for non-CN resolution. The first entry
	// gets detour=out (goes through the airport proxy).
	ProxyDoH []string `yaml:"proxy_doh"`
	// BootstrapResolver is an IP literal used to resolve any DoH/DoT host
	// names without chicken-and-egg (e.g. "udp://119.29.29.29").
	BootstrapResolver string `yaml:"bootstrap_resolver"`
}

type RouteConfig struct {
	GeositeURL string `yaml:"geosite_url"`
	GeoIPURL   string `yaml:"geoip_url"`
}

// URLTestConfig drives both sing-box's native urltest outbound and the
// in-process watchdog that supplements urltest's coarse interval-based check
// with a fast-path failure detector.
type URLTestConfig struct {
	// Interval = sing-box urltest periodic full-pool re-test (default 3m).
	Interval time.Duration `yaml:"interval"`
	// Tolerance = ms hysteresis: only switch if a new candidate is faster by
	// this much (default 50).
	Tolerance int `yaml:"tolerance"`
	// ProbeURL = the URL probed for latency. Default gstatic.com/generate_204
	// because it must traverse GFW (which is what we want to verify).
	ProbeURL string `yaml:"probe_url"`
	// NodePattern is a Go-regexp; only outbounds whose tag matches are added
	// to the urltest pool. Empty = every airport node is a candidate.
	NodePattern string `yaml:"node_pattern"`

	Watchdog WatchdogConfig `yaml:"watchdog"`
}

// WatchdogConfig drives the in-process active health checker. Every Interval
// it probes the currently selected urltest member; FailThreshold consecutive
// failures triggers a forced full re-test (clash-api /proxies/urltest/delay)
// which makes sing-box re-pick the fastest reachable node.
type WatchdogConfig struct {
	Enabled       bool          `yaml:"enabled"`
	Interval      time.Duration `yaml:"interval"`       // default 5s
	Timeout       time.Duration `yaml:"timeout"`        // default 3s
	FailThreshold int           `yaml:"fail_threshold"` // default 3
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.API.Listen == "" {
		// 18080 (not 8080) — feilian-sentry on the forwarding node already
		// owns 127.0.0.1:8080.
		c.API.Listen = "127.0.0.1:18080"
	}
	if c.Subscribe.RefreshInterval == 0 {
		c.Subscribe.RefreshInterval = 30 * time.Minute
	}
	if c.Subscribe.HTTPTimeout == 0 {
		c.Subscribe.HTTPTimeout = 30 * time.Second
	}
	if c.Subscribe.UserAgent == "" {
		// Most subscription providers gate format by UA. sing-box UA gets us
		// native sing-box JSON, which is the format our renderer targets.
		c.Subscribe.UserAgent = "sing-box/1.8.0"
	}
	c.SingBox.ApplyDefaults()
}

// ApplyDefaults fills in defaults for the SingBox subtree. Public so callers
// that construct SingBoxConfig directly (e.g. cmd/selftest) can invoke it.
func (c *SingBoxConfig) ApplyDefaults() {
	if c.ConfigPath == "" {
		c.ConfigPath = "/etc/leap/singbox/config.json"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.ClashAPI.ExternalController == "" {
		c.ClashAPI.ExternalController = "127.0.0.1:9090"
	}

	// TUN defaults — only used when TUN.Enabled is true.
	if c.TUN.InterfaceName == "" {
		c.TUN.InterfaceName = "utun-leap"
	}
	if c.TUN.Address == "" {
		c.TUN.Address = "172.19.0.1/30"
	}
	if c.TUN.FWMark == 0 {
		c.TUN.FWMark = 0x42
	}
	if c.TUN.RoutingTable == 0 {
		c.TUN.RoutingTable = 100
	}

	// DNS defaults.
	if c.DNS.FakeIPRange == "" {
		c.DNS.FakeIPRange = "198.18.0.0/15"
	}
	if len(c.DNS.CNDoH) == 0 {
		// Tencent first (faster from FeiLian node measured 2026-05-27),
		// AliDNS as fallback.
		c.DNS.CNDoH = []string{
			"https://doh.pub/dns-query",
			"https://dns.alidns.com/dns-query",
		}
	}
	if len(c.DNS.ProxyDoH) == 0 {
		// Cloudflare DoT — node-direct reachable per measurement.
		// doh.pub fallback because it returns un-poisoned answers for
		// overseas domains (verified 2026-05-27).
		c.DNS.ProxyDoH = []string{
			"tls://1.1.1.1:853",
			"https://doh.pub/dns-query",
		}
	}
	if c.DNS.BootstrapResolver == "" {
		c.DNS.BootstrapResolver = "udp://119.29.29.29"
	}

	// Route defaults — sagernet's official rule sets.
	if c.Route.GeositeURL == "" {
		c.Route.GeositeURL = "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs"
	}
	if c.Route.GeoIPURL == "" {
		c.Route.GeoIPURL = "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-cn.srs"
	}

	// URLTest defaults.
	if c.URLTest.Interval == 0 {
		c.URLTest.Interval = 3 * time.Minute
	}
	if c.URLTest.Tolerance == 0 {
		c.URLTest.Tolerance = 50
	}
	if c.URLTest.ProbeURL == "" {
		c.URLTest.ProbeURL = "https://www.gstatic.com/generate_204"
	}
	// Watchdog defaults — enabled by default once urltest is in place.
	if c.URLTest.Watchdog.Interval == 0 {
		c.URLTest.Watchdog.Interval = 5 * time.Second
	}
	if c.URLTest.Watchdog.Timeout == 0 {
		c.URLTest.Watchdog.Timeout = 3 * time.Second
	}
	if c.URLTest.Watchdog.FailThreshold == 0 {
		c.URLTest.Watchdog.FailThreshold = 3
	}
}
