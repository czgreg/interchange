package mihomo

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests replaced an earlier set written against
// buildProxyServerNameserver, which appended a plain-UDP upstream to the
// proxy-server-nameserver LIST. That function is gone, and so are its
// tests, because the model behind them was wrong: they asserted "order
// matters: CN DoH first preserves the pre-existing resolution path", but
// mihomo queries all psn upstreams in PARALLEL and takes the first
// non-error answer, and NXDOMAIN does not count as an error there. List
// order is cosmetic, and a fast NXDOMAIN upstream defeats a healthy one in
// the same list (verified on the node: 10/10 dials failed with a healthy
// 119.29.29.29 present). See buildProxyServerNameserverPolicy.

// End-to-end through the renderer: with suffixes configured, psn stays
// CNDoH-only and the policy block carries the pinned suffix. This is the
// shape verified against `mihomo -t` on node 92 before rollout.
func TestRenderer_PSNPolicyEmittedWhenSuffixesConfigured(t *testing.T) {
	r := newTestRenderer(t)
	r.cfg.DNS.CNDoH = []string{"https://doh.pub/dns-query"}
	r.cfg.DNS.ProxyDoH = []string{"tls://1.1.1.1:853"}
	r.cfg.DNS.NodeResolver = []string{"udp://119.29.29.29"}
	r.cfg.DNS.NodeResolverSuffixes = []string{"edg3.org"}

	body, err := r.Write(sampleOutbounds())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dns, _ := doc["dns"].(map[string]any)

	// psn must NOT have gained the UDP upstream — that is the reverted
	// approach, and having both would reintroduce the race.
	psn, _ := dns["proxy-server-nameserver"].([]any)
	if len(psn) != 1 || psn[0] != "https://doh.pub/dns-query" {
		t.Errorf("proxy-server-nameserver = %v, want cnDoH only", psn)
	}

	pol, ok := dns["proxy-server-nameserver-policy"].(map[string]any)
	if !ok {
		t.Fatalf("proxy-server-nameserver-policy missing or wrong type: %#v",
			dns["proxy-server-nameserver-policy"])
	}
	ups, ok := pol["+.edg3.org"].([]any)
	if !ok {
		t.Fatalf("policy key +.edg3.org missing; got %v", pol)
	}
	if len(ups) != 1 || ups[0] != "udp://119.29.29.29" {
		t.Errorf("policy upstreams = %v, want [udp://119.29.29.29]", ups)
	}
}

func TestDedupeUpstreams(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"preserves order", []string{"a", "b"}, []string{"a", "b"}},
		{"drops duplicates", []string{"a", "b", "a"}, []string{"a", "b"}},
		{"drops empties", []string{"", "a", ""}, []string{"a"}},
		{"empty input", nil, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupeUpstreams(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// No suffixes configured → no policy block at all, so the rendered config
// carries no plain-UDP resolution path. This is the default and must stay
// the default.
func TestBuildProxyServerNameserverPolicy_EmptyWithoutSuffixes(t *testing.T) {
	if got := buildProxyServerNameserverPolicy(nil, []string{"udp://119.29.29.29"}); got != nil {
		t.Errorf("expected nil policy with no suffixes, got %v", got)
	}
	if got := buildProxyServerNameserverPolicy([]string{}, []string{"udp://119.29.29.29"}); got != nil {
		t.Errorf("expected nil policy with empty suffixes, got %v", got)
	}
}

// Suffixes but no upstreams would emit a policy pointing nowhere, which
// mihomo would reject or silently treat as no-resolver.
func TestBuildProxyServerNameserverPolicy_EmptyWithoutUpstreams(t *testing.T) {
	if got := buildProxyServerNameserverPolicy([]string{"edg3.org"}, nil); got != nil {
		t.Errorf("expected nil policy with no upstreams, got %v", got)
	}
	if got := buildProxyServerNameserverPolicy([]string{"edg3.org"}, []string{""}); got != nil {
		t.Errorf("empty-string upstream must not produce a policy, got %v", got)
	}
}

// All three spellings an operator might write must normalize to mihomo's
// wildcard key form, so the same domain cannot end up with two keys.
func TestBuildProxyServerNameserverPolicy_NormalizesSuffixForms(t *testing.T) {
	ups := []string{"udp://119.29.29.29"}
	for _, in := range []string{"edg3.org", ".edg3.org", "+.edg3.org"} {
		pol := buildProxyServerNameserverPolicy([]string{in}, ups)
		if len(pol) != 1 {
			t.Fatalf("%q: expected 1 key, got %v", in, pol)
		}
		if _, ok := pol["+.edg3.org"]; !ok {
			t.Errorf("%q: expected key %q, got %v", in, "+.edg3.org", pol)
		}
	}
}

func TestBuildProxyServerNameserverPolicy_MultipleSuffixesShareUpstreams(t *testing.T) {
	ups := []string{"udp://119.29.29.29", "udp://223.5.5.5"}
	pol := buildProxyServerNameserverPolicy([]string{"edg3.org", "ashnet.one"}, ups)
	if len(pol) != 2 {
		t.Fatalf("expected 2 keys, got %v", pol)
	}
	for _, k := range []string{"+.edg3.org", "+.ashnet.one"} {
		v, ok := pol[k]
		if !ok {
			t.Fatalf("missing key %q in %v", k, pol)
		}
		got, ok := v.([]string)
		if !ok {
			t.Fatalf("key %q: value is %T, want []string", k, v)
		}
		if len(got) != 2 || got[0] != ups[0] || got[1] != ups[1] {
			t.Errorf("key %q: got %v, want %v", k, got, ups)
		}
	}
}

// Duplicated upstreams within node_resolver must not reach the rendered
// config — mihomo would query the same server twice in the same race.
func TestBuildProxyServerNameserverPolicy_DedupesUpstreams(t *testing.T) {
	pol := buildProxyServerNameserverPolicy(
		[]string{"edg3.org"},
		[]string{"udp://119.29.29.29", "udp://119.29.29.29"})
	got := pol["+.edg3.org"].([]string)
	if len(got) != 1 {
		t.Errorf("expected deduped upstreams, got %v", got)
	}
}
