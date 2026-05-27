package singbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

// TestRenderedConfigPassesSingBoxCheck runs `sing-box check` against the
// rendered config to catch schema breakage. Requires sing-box 1.10.x on PATH
// (the version we pin to in production for clash-api stability) — newer
// sing-box (1.13+) has removed the inbound types we use, and the check is
// skipped in that case so dev machines with `brew install sing-box` (which
// pulls latest) don't fail.
//
// To force-enable on a non-pinned version, set SING_BOX_BIN to a 1.10.x binary.
func TestRenderedConfigPassesSingBoxCheck(t *testing.T) {
	bin := os.Getenv("SING_BOX_BIN")
	if bin == "" {
		bin = "sing-box"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not on PATH; skipping schema check", bin)
	}
	verOut, _ := exec.Command(bin, "version").Output()
	if !strings.Contains(string(verOut), "version 1.10.") && os.Getenv("SING_BOX_BIN") == "" {
		t.Skipf("sing-box version mismatch (want 1.10.x for our pinned schema); got: %s", strings.SplitN(string(verOut), "\n", 2)[0])
	}

	dir := t.TempDir()
	cfg := config.SingBoxConfig{
		ConfigPath: filepath.Join(dir, "config.json"),
	}
	cfg.ApplyDefaults()
	cfg.TUN.Enabled = true
	r := NewRenderer(cfg).WithNode(config.NodeConfig{
		ClientSubnet:  "10.8.11.0/24",
		Tun0GatewayIP: "10.8.11.1",
		EgressIface:   "ens18",
	})
	outs := []subscribe.Outbound{
		{"type": "shadowsocks", "tag": "n1", "server": "1.2.3.4", "server_port": 8388, "method": "aes-128-gcm", "password": "abcdef"},
		{"type": "trojan", "tag": "n2", "server": "5.6.7.8", "server_port": 443, "password": "p1"},
	}
	if _, err := r.Write(outs); err != nil {
		t.Fatalf("render: %v", err)
	}
	cmd := exec.Command(bin, "check", "-c", cfg.ConfigPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sing-box check failed: %v\n%s", err, out)
	}
}
