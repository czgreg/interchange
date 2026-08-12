package config

// The shipped example config must actually load. It is the template
// operators copy to /etc/leap/gateway.yaml, and it is the only executable
// documentation for the schema — a field renamed in Go without updating it
// leaves the example silently wrong.
//
// This test was added 2026-08-12 after finding the example had been
// unparseable: `refresh_interval: 0` cannot unmarshal into a
// time.Duration, so anyone copying the file verbatim got
// "parse config: yaml: unmarshal errors" on first start.

import (
	"testing"
	"time"
)

func TestExampleConfigLoads(t *testing.T) {
	c, err := Load("../../configs/gateway.example.yaml")
	if err != nil {
		t.Fatalf("configs/gateway.example.yaml must load: %v", err)
	}

	// Spot-check fields whose types are easy to get wrong in YAML
	// (durations, string slices) rather than asserting the whole tree.
	if c.Subscribe.HTTPTimeout != 30*time.Second {
		t.Errorf("subscribe.http_timeout = %v, want 30s", c.Subscribe.HTTPTimeout)
	}
	if c.API.Listen == "" {
		t.Error("api.listen must be set")
	}

	// The example must ship the SAFE defaults: loopback-only API, and no
	// plain-UDP node-resolver policy. An example that ships 0.0.0.0 with an
	// empty token is how a node ends up with an open control plane.
	if c.API.Listen != "127.0.0.1:18080" {
		t.Errorf("api.listen = %q, want loopback default in the example", c.API.Listen)
	}
	if c.API.Token != "" {
		t.Error("example must not ship a real token")
	}
	if len(c.API.AllowFrom) != 0 {
		t.Errorf("example must ship an empty api.allow_from (loopback only), got %v", c.API.AllowFrom)
	}
	if len(c.DataPlane.DNS.NodeResolverSuffixes) != 0 {
		t.Errorf("example must ship no node_resolver_suffixes (all node lookups over DoH), got %v",
			c.DataPlane.DNS.NodeResolverSuffixes)
	}
}
