package api

import (
	"strings"
	"testing"
)

func TestMaskURLToken(t *testing.T) {
	cases := []struct {
		in       string
		want     string // must contain
		mustNot  string // must not contain (the original token)
	}{
		{
			in:      "https://yuyun.example/sub?token=3f18a3cc5c91219b4fa90115d4f78960",
			want:    "3f18***8960",
			mustNot: "3f18a3cc5c91219b4fa90115d4f78960",
		},
		{
			in:     "https://example.com/sub",
			want:   "https://example.com/sub",
			mustNot: "***",
		},
		{
			in:     "https://example.com/sub?token=short",
			want:   "****",
			mustNot: "short",
		},
	}
	for _, c := range cases {
		got := maskURLToken(c.in)
		if !strings.Contains(got, c.want) {
			t.Errorf("maskURLToken(%q) = %q, want substring %q", c.in, got, c.want)
		}
		if c.mustNot != "" && c.mustNot != "***" && strings.Contains(got, c.mustNot) {
			t.Errorf("maskURLToken(%q) = %q leaks %q", c.in, got, c.mustNot)
		}
	}
}

func TestValidateDomainSuffix(t *testing.T) {
	good := []string{"example.com", "a.b.c.com", "claude.ai", "下载.中国"}
	for _, s := range good {
		if err := validateDomainSuffix(s); err != nil {
			t.Errorf("validateDomainSuffix(%q) unexpected err: %v", s, err)
		}
	}
	bad := []string{"", "no_dot", "https://example.com", "exa mple.com", ".leading.com", "trailing.dot.", "/etc/passwd"}
	for _, s := range bad {
		if err := validateDomainSuffix(s); err == nil {
			t.Errorf("validateDomainSuffix(%q) should have errored", s)
		}
	}
}

func TestValidateSubscriptionURL(t *testing.T) {
	good := []string{
		"https://example.com/sub",
		"http://example.com/sub?token=x",
	}
	for _, s := range good {
		if err := validateSubscriptionURL(s); err != nil {
			t.Errorf("validateSubscriptionURL(%q) unexpected err: %v", s, err)
		}
	}
	bad := []string{
		"",
		"not a url",
		"ftp://example.com/sub",
		"https:///nopath",
	}
	for _, s := range bad {
		if err := validateSubscriptionURL(s); err == nil {
			t.Errorf("validateSubscriptionURL(%q) should have errored", s)
		}
	}
}
