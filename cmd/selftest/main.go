// selftest is a one-shot CLI: fetch a subscription URL or replay a saved
// body, run it through the real parser + renderer, and print
// non-sensitive stats. Use it to validate a new subscription provider
// before wiring it into the production config.
//
//	go run ./cmd/selftest --url "https://..." [--ua sing-box/1.8.0]
//	go run ./cmd/selftest --file ./body.json --format singbox      # offline replay
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/leap-gateway/leap-gateway/internal/config"
	"github.com/leap-gateway/leap-gateway/internal/singbox"
	"github.com/leap-gateway/leap-gateway/internal/subscribe"
)

func main() {
	url := flag.String("url", "", "subscription URL")
	file := flag.String("file", "", "read body from file instead of fetching (offline replay)")
	ua := flag.String("ua", "sing-box/1.8.0", "User-Agent header")
	name := flag.String("name", "selftest", "subscription name (tag prefix)")
	format := flag.String("format", "auto", "auto | clash | singbox | uri | sip008")
	timeout := flag.Duration("timeout", 30*time.Second, "fetch timeout")
	render := flag.String("render", "", "if set, render sing-box config to this path")
	flag.Parse()

	if *url == "" && *file == "" {
		fmt.Fprintln(os.Stderr, "need --url or --file")
		os.Exit(2)
	}

	results, allOutbounds, err := run(*url, *file, *ua, *name, *format, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	for _, r := range results {
		fmt.Printf("subscription: %s (format=%s, nodes=%d)\n", r.Name, r.Format, len(r.Outbounds))
		typeCount := map[string]int{}
		for _, o := range r.Outbounds {
			typeCount[o.Type()]++
		}
		types := make([]string, 0, len(typeCount))
		for t := range typeCount {
			types = append(types, t)
		}
		sort.Strings(types)
		for _, t := range types {
			fmt.Printf("  %-14s %d\n", t, typeCount[t])
		}
		fmt.Println("  sample tags:")
		n := 5
		if n > len(r.Outbounds) {
			n = len(r.Outbounds)
		}
		for _, o := range r.Outbounds[:n] {
			fmt.Printf("    - %s [%s]\n", o.Tag(), o.Type())
		}
	}

	if *render != "" {
		rcfg := config.SingBoxConfig{
			ConfigPath:  *render,
			SOCKSListen: "0.0.0.0:1080",
			HTTPListen:  "0.0.0.0:1081",
		}
		rcfg.ApplyDefaults()
		// No NodeConfig — selftest renders in lab mode (no TUN, no DNS-direct
		// inbound). Output still passes `sing-box check` for static validation.
		data, err := singbox.NewRenderer(rcfg).Write(allOutbounds)
		if err != nil {
			fmt.Fprintf(os.Stderr, "render error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("rendered %d bytes to %s\n", len(data), *render)
	}
}

// run isolates the fetch/parse/aggregate path so main is just CLI plumbing.
func run(url, file, ua, name, format string, timeout time.Duration) ([]subscribe.SubscriptionResult, []subscribe.Outbound, error) {
	if file != "" {
		body, err := os.ReadFile(file)
		if err != nil {
			return nil, nil, fmt.Errorf("read file: %w", err)
		}
		r, err := subscribe.ParseBytes(name, format, body)
		if err != nil {
			return nil, nil, err
		}
		return []subscribe.SubscriptionResult{r}, r.Outbounds, nil
	}
	mgr := subscribe.NewManagerWithFetch(
		[]config.SubscriptionEntry{{Name: name, URL: url, Format: format, Enabled: true}},
		timeout, ua,
	)
	ctx, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
	defer cancel()
	results, err := mgr.Refresh(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "refresh warn: %v\n", err)
	}
	if len(results) == 0 {
		return nil, nil, fmt.Errorf("no subscription produced any nodes")
	}
	return results, mgr.AllOutbounds(), nil
}
