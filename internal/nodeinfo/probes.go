package nodeinfo

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// unameR returns "uname -r" — kernel release. Linux-only; on other OSes
// returns runtime.GOOS for diagnostic purposes.
func unameR() string {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// osPretty pulls PRETTY_NAME from /etc/os-release. Empty if file missing.
func osPretty() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			v := strings.TrimPrefix(line, "PRETTY_NAME=")
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// readUptime parses /proc/uptime — first field is seconds since boot.
func readUptime() int64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

// listInterfaces returns named interfaces with their primary IPv4 address.
// Skips loopback and interfaces with no IPv4. Order matches OS enumeration.
func listInterfaces() []Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]Interface, 0, len(ifaces))
	for _, ifa := range ifaces {
		if ifa.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifa.Addrs()
		if err != nil {
			continue
		}
		var v4 string
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ipnet.IP.To4() != nil {
				v4 = ipnet.IP.String()
				break
			}
		}
		if v4 == "" {
			continue
		}
		out = append(out, Interface{Name: ifa.Name, IPv4: v4})
	}
	return out
}

// systemdActive: `systemctl is-active --quiet <unit>`. Boolean result.
func systemdActive(ctx context.Context, unit string) bool {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "systemctl", "is-active", "--quiet", unit)
	return cmd.Run() == nil
}

// systemdState: `systemctl is-active <unit>` — returns the textual state
// ("active", "inactive", "failed", "activating", ...). Empty string on error.
func systemdState(ctx context.Context, unit string) string {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(c, "systemctl", "is-active", unit).Output()
	return strings.TrimSpace(string(out))
}

// nftFeiLianChains lists chain names in `table ip nat` that start with
// FEILIAN_. Empty slice if the table doesn't exist or nft fails.
func nftFeiLianChains(ctx context.Context) []string {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "nft", "list", "table", "ip", "nat").Output()
	if err != nil {
		return nil
	}
	var chains []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "chain ") {
			continue
		}
		// "chain FEILIAN_PROXY {"
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		name := parts[1]
		if strings.HasPrefix(name, "FEILIAN_") {
			chains = append(chains, name)
		}
	}
	return chains
}

// singboxVersion: `sing-box version` — first line, trimmed.
// Typical output: "sing-box version 1.10.7".
func singboxVersion(ctx context.Context) string {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "/usr/local/bin/sing-box", "version").Output()
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(out), "\n")
	first = strings.TrimSpace(first)
	first = strings.TrimPrefix(first, "sing-box version ")
	return first
}
