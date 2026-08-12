package mihomo

import "testing"

// buildProxyServerNameserver must append node-resolver upstreams after CN
// DoH, dedupe across both lists, and drop empties. Order matters: CN DoH
// first preserves the pre-existing resolution path; the UDP upstream is a
// fallback for DoH frontends that answer NXDOMAIN for live node hostnames.
func TestBuildProxyServerNameserver(t *testing.T) {
	tests := []struct {
		name         string
		cnDoH        []string
		nodeResolver []string
		want         []string
	}{
		{
			name:         "appends node resolver after cn doh",
			cnDoH:        []string{"https://doh.pub/dns-query", "https://dns.alidns.com/dns-query"},
			nodeResolver: []string{"udp://119.29.29.29"},
			want:         []string{"https://doh.pub/dns-query", "https://dns.alidns.com/dns-query", "udp://119.29.29.29"},
		},
		{
			name:         "dedupes overlap between lists",
			cnDoH:        []string{"https://doh.pub/dns-query"},
			nodeResolver: []string{"https://doh.pub/dns-query", "udp://119.29.29.29"},
			want:         []string{"https://doh.pub/dns-query", "udp://119.29.29.29"},
		},
		{
			name:         "identical lists collapse to cn doh only (opt-out path)",
			cnDoH:        []string{"https://doh.pub/dns-query"},
			nodeResolver: []string{"https://doh.pub/dns-query"},
			want:         []string{"https://doh.pub/dns-query"},
		},
		{
			name:         "drops empty strings",
			cnDoH:        []string{"", "https://doh.pub/dns-query"},
			nodeResolver: []string{"", "udp://119.29.29.29"},
			want:         []string{"https://doh.pub/dns-query", "udp://119.29.29.29"},
		},
		{
			name:         "dedupes within node resolver itself",
			cnDoH:        []string{"https://doh.pub/dns-query"},
			nodeResolver: []string{"udp://119.29.29.29", "udp://119.29.29.29"},
			want:         []string{"https://doh.pub/dns-query", "udp://119.29.29.29"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildProxyServerNameserver(tc.cnDoH, tc.nodeResolver)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("index %d: got %q, want %q (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}
