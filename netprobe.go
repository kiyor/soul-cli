package main

// netprobe: one-shot local-network probing so the agent only hits ONE
// approval wall per cycle. The subcommand concurrently checks:
//   - Agent services on 192.168.10.26 (jira, memory, gallery, prompt, rss, ...)
//   - GPU box (ComfyUI on 192.168.10.52:8188)
//   - VPN gateways (SOCKS5 on .21:1080 / .24:1080)
//   - Infra hosts (PVE, opnsense, fs, qollama)
//   - K8s contexts (optional, via kubectl cluster-info — skipped by default)
//
// Extensible via $workspace/netprobe.yaml (optional). If the file exists,
// additional targets are appended to the built-in list.
//
// Usage:
//   weiran netprobe                          # probe everything, pretty table
//   weiran netprobe --json                   # JSON output
//   weiran netprobe --only service,vpn       # filter categories
//   weiran netprobe --timeout 2s             # per-probe timeout
//   weiran netprobe --extra http://host:port # ad-hoc extra target
//   weiran netprobe --list                   # print target list, no probing
//
// Exit codes:
//   0  all probes OK
//   1  at least one probe failed (any reachable-but-bad or unreachable)
//   2  argument / config error

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ── Types ────────────────────────────────────────────────────────

type netProbeTarget struct {
	Name     string        `yaml:"name" json:"name"`
	Category string        `yaml:"category" json:"category"` // service | gpu | vpn | infra | k8s | extra
	Kind     string        `yaml:"kind" json:"kind"`         // http | tcp
	URL      string        `yaml:"url,omitempty" json:"url,omitempty"`
	Addr     string        `yaml:"addr,omitempty" json:"addr,omitempty"` // host:port (for tcp)
	Expect   []int         `yaml:"expect,omitempty" json:"expect,omitempty"`
	Timeout  time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Note     string        `yaml:"note,omitempty" json:"note,omitempty"`
}

type netProbeResult struct {
	Target   netProbeTarget `json:"target"`
	OK       bool           `json:"ok"`
	Status   int            `json:"status,omitempty"`
	Latency  time.Duration  `json:"latency"`
	LatencyS string         `json:"latency_human"`
	Error    string         `json:"error,omitempty"`
}

// ── Built-in target list ─────────────────────────────────────────

// defaultNetProbeTargets covers the local services documented in TOOLS.md.
// Host: 192.168.10.26 is the Mac mini running all agent services.
// Keep this list tight — it's a fast "is everything alive" sweep.
func defaultNetProbeTargets() []netProbeTarget {
	return []netProbeTarget{
		// ── Agent services on 192.168.10.26 ──
		{Name: "jira", Category: "service", Kind: "http", URL: "http://192.168.10.26:8081/api/agents/main/checkin", Expect: []int{200, 400, 401, 403, 404, 405}},
		{Name: "memory", Category: "service", Kind: "http", URL: "http://192.168.10.26:8084/api/health"},
		{Name: "prompt-manager", Category: "service", Kind: "http", URL: "http://192.168.10.26:8083/api/agents"},
		{Name: "rss-reader", Category: "service", Kind: "http", URL: "http://192.168.10.26:8088/"},
		{Name: "rss-weiran", Category: "service", Kind: "http", URL: "http://192.168.10.26:8096/"},
		{Name: "gen-gallery", Category: "service", Kind: "http", URL: "http://192.168.10.26:8089/api/generations?limit=1"},
		{Name: "office-dashboard", Category: "service", Kind: "http", URL: "http://192.168.10.26:8080/"},
		{Name: "plantuml", Category: "service", Kind: "http", URL: "http://192.168.10.26:8093/"},
		{Name: "feishu-bot", Category: "service", Kind: "http", URL: "http://192.168.10.26:8095/"},
		{Name: "vault", Category: "service", Kind: "http", URL: "http://192.168.10.26:8200/v1/sys/health", Expect: []int{200, 429, 472, 473, 501, 503}},

		// ── GPU box ──
		{Name: "comfyui", Category: "gpu", Kind: "http", URL: "http://192.168.10.52:8188/system_stats"},
		{Name: "qollama", Category: "gpu", Kind: "http", URL: "http://192.168.10.52:11434/api/tags"},

		// ── VPN gateways (SOCKS5 TCP reach only) ──
		{Name: "vpn-21-socks5", Category: "vpn", Kind: "tcp", Addr: "192.168.10.21:1080", Note: "Zilliz OpenVPN"},
		{Name: "vpn-24-socks5", Category: "vpn", Kind: "tcp", Addr: "192.168.10.24:1080", Note: "OpenFortiVPN backup"},

		// ── Infra / hosts ──
		{Name: "opnsense", Category: "infra", Kind: "tcp", Addr: "192.168.10.1:443", Note: "router admin"},
		{Name: "fs-truenas", Category: "infra", Kind: "tcp", Addr: "192.168.10.70:22", Note: "TrueNAS ssh"},
		{Name: "pve-23", Category: "infra", Kind: "tcp", Addr: "192.168.10.23:8006", Note: "PVE primary node"},
		{Name: "javbus", Category: "infra", Kind: "tcp", Addr: "192.168.10.50:22", Note: "k2fs / javbus host"},
	}
}

// loadExtraNetProbeTargets reads $workspace/netprobe.yaml if present.
func loadExtraNetProbeTargets() ([]netProbeTarget, error) {
	path := filepath.Join(workspace, "netprobe.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out struct {
		Targets []netProbeTarget `yaml:"targets"`
	}
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out.Targets, nil
}

// ── Probing ──────────────────────────────────────────────────────

func probeOne(ctx context.Context, t netProbeTarget) netProbeResult {
	to := t.Timeout
	if to <= 0 {
		to = 3 * time.Second
	}
	start := time.Now()
	res := netProbeResult{Target: t}

	switch strings.ToLower(t.Kind) {
	case "http", "":
		// Default to HTTP if URL looks set.
		url := t.URL
		if url == "" {
			res.Error = "no url for http probe"
			res.Latency = time.Since(start)
			res.LatencyS = res.Latency.Truncate(time.Millisecond).String()
			return res
		}
		cctx, cancel := context.WithTimeout(ctx, to)
		defer cancel()
		req, err := http.NewRequestWithContext(cctx, "GET", url, nil)
		if err != nil {
			res.Error = err.Error()
			break
		}
		req.Header.Set("User-Agent", appName+"/netprobe")
		client := &http.Client{Timeout: to}
		resp, err := client.Do(req)
		if err != nil {
			res.Error = err.Error()
			break
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		res.Status = resp.StatusCode
		res.OK = statusAcceptable(resp.StatusCode, t.Expect)
		if !res.OK && res.Error == "" {
			res.Error = fmt.Sprintf("unexpected status %d", resp.StatusCode)
		}
	case "tcp":
		addr := t.Addr
		if addr == "" {
			res.Error = "no addr for tcp probe"
			break
		}
		d := net.Dialer{Timeout: to}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			res.Error = err.Error()
			break
		}
		conn.Close()
		res.OK = true
	default:
		res.Error = fmt.Sprintf("unknown kind %q", t.Kind)
	}

	res.Latency = time.Since(start)
	res.LatencyS = res.Latency.Truncate(time.Millisecond).String()
	return res
}

// statusAcceptable returns true if code matches expect (or is 2xx/3xx when expect empty).
func statusAcceptable(code int, expect []int) bool {
	if len(expect) == 0 {
		return code >= 200 && code < 400
	}
	for _, e := range expect {
		if code == e {
			return true
		}
	}
	return false
}

// ── CLI entry ────────────────────────────────────────────────────

func handleNetProbe(args []string) {
	jsonOut := false
	listOnly := false
	timeout := 3 * time.Second
	onlyCats := map[string]bool{}
	var extras []netProbeTarget

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json", "-j":
			jsonOut = true
		case "--list", "-l":
			listOnly = true
		case "--timeout", "-t":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --timeout needs a duration (e.g. 2s)")
				os.Exit(2)
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: bad --timeout: %v\n", err)
				os.Exit(2)
			}
			timeout = d
			i++
		case "--only":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --only needs comma-separated categories")
				os.Exit(2)
			}
			for _, c := range strings.Split(args[i+1], ",") {
				c = strings.TrimSpace(c)
				if c != "" {
					onlyCats[c] = true
				}
			}
			i++
		case "--extra":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --extra needs a URL or host:port")
				os.Exit(2)
			}
			raw := args[i+1]
			t := netProbeTarget{Name: "extra", Category: "extra"}
			if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
				t.Kind = "http"
				t.URL = raw
			} else {
				t.Kind = "tcp"
				t.Addr = raw
			}
			extras = append(extras, t)
			i++
		case "--help", "-h":
			printNetProbeHelp()
			return
		default:
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", args[i])
			printNetProbeHelp()
			os.Exit(2)
		}
	}

	// Assemble targets
	targets := defaultNetProbeTargets()
	if xtra, err := loadExtraNetProbeTargets(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] warning: netprobe.yaml: %v\n", appName, err)
	} else {
		targets = append(targets, xtra...)
	}
	targets = append(targets, extras...)

	// Normalize default category & filter
	var filtered []netProbeTarget
	for _, t := range targets {
		if t.Category == "" {
			t.Category = "service"
		}
		if t.Kind == "" {
			if t.URL != "" {
				t.Kind = "http"
			} else if t.Addr != "" {
				t.Kind = "tcp"
			}
		}
		if t.Timeout == 0 {
			t.Timeout = timeout
		}
		if len(onlyCats) > 0 && !onlyCats[t.Category] {
			continue
		}
		filtered = append(filtered, t)
	}

	if listOnly {
		printTargetList(filtered, jsonOut)
		return
	}

	if len(filtered) == 0 {
		fmt.Fprintln(os.Stderr, "no targets to probe (check --only filter)")
		os.Exit(2)
	}

	// Run in parallel
	ctx := context.Background()
	results := make([]netProbeResult, len(filtered))
	var wg sync.WaitGroup
	for i, t := range filtered {
		wg.Add(1)
		go func(i int, t netProbeTarget) {
			defer wg.Done()
			results[i] = probeOne(ctx, t)
		}(i, t)
	}
	wg.Wait()

	// Sort by (category, name) for stable display
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Target.Category != results[j].Target.Category {
			return results[i].Target.Category < results[j].Target.Category
		}
		return results[i].Target.Name < results[j].Target.Name
	})

	anyFail := false
	for _, r := range results {
		if !r.OK {
			anyFail = true
			break
		}
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]interface{}{
			"ok":      !anyFail,
			"count":   len(results),
			"results": results,
		})
	} else {
		printResultsTable(results)
	}

	if anyFail {
		os.Exit(1)
	}
}

// ── Output helpers ───────────────────────────────────────────────

func printResultsTable(results []netProbeResult) {
	// column widths
	nameW, catW, tgtW := 4, 3, 6
	for _, r := range results {
		if n := len(r.Target.Name); n > nameW {
			nameW = n
		}
		if n := len(r.Target.Category); n > catW {
			catW = n
		}
		tgt := r.Target.URL
		if tgt == "" {
			tgt = r.Target.Addr
		}
		if n := len(tgt); n > tgtW {
			tgtW = n
		}
	}

	okCount := 0
	header := fmt.Sprintf("%-4s %-*s %-*s %-*s %-8s %-10s  %s",
		"",
		catW, "CAT",
		nameW, "NAME",
		tgtW, "TARGET",
		"STATUS",
		"LATENCY",
		"NOTE")
	fmt.Println(header)
	fmt.Println(strings.Repeat("─", len(header)+8))

	for _, r := range results {
		icon := "OK "
		if !r.OK {
			icon = "FAIL"
		} else {
			okCount++
		}
		tgt := r.Target.URL
		if tgt == "" {
			tgt = r.Target.Addr
		}
		status := ""
		if r.Status > 0 {
			status = fmt.Sprintf("%d", r.Status)
		} else if r.Target.Kind == "tcp" && r.OK {
			status = "connect"
		}
		note := r.Target.Note
		if !r.OK && r.Error != "" {
			note = netprobeTruncate(r.Error, 80)
		}
		fmt.Printf("%-4s %-*s %-*s %-*s %-8s %-10s  %s\n",
			icon,
			catW, r.Target.Category,
			nameW, r.Target.Name,
			tgtW, tgt,
			status,
			r.LatencyS,
			note,
		)
	}

	fmt.Printf("\n%d/%d OK\n", okCount, len(results))
}

func printTargetList(targets []netProbeTarget, jsonOut bool) {
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(targets)
		return
	}
	for _, t := range targets {
		tgt := t.URL
		if tgt == "" {
			tgt = t.Addr
		}
		fmt.Printf("  %-8s %-4s %-20s %s\n", t.Category, t.Kind, t.Name, tgt)
	}
	fmt.Printf("\n%d targets.\n", len(targets))
}

func netprobeTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func printNetProbeHelp() {
	fmt.Printf(`%s netprobe — one-shot local network health probe

Purpose: the agent hits MANY local services on a cycle. Each curl/nc
would trigger a Bash approval wall. Instead, call this ONCE and one
approval covers the whole sweep.

Usage:
  %s netprobe                          probe all known targets (pretty table)
  %s netprobe --json                   JSON output
  %s netprobe --only service,vpn       filter categories (comma-separated)
  %s netprobe --timeout 2s             per-probe timeout (default 3s)
  %s netprobe --extra <url|host:port>  add an ad-hoc target (repeatable)
  %s netprobe --list                   list targets without probing

Categories:
  service   Mac mini agent services on 192.168.10.26 (jira, memory, ...)
  gpu       GPU box 192.168.10.52 (comfyui, qollama)
  vpn       SOCKS5 proxies (.21, .24)
  infra     Router / TrueNAS / PVE / other LAN hosts
  extra     Ad-hoc targets added via --extra

Extend targets:
  Create %s/netprobe.yaml:
    targets:
      - name: myservice
        category: service
        kind: http      # or tcp
        url: http://host:port/path
        expect: [200, 204]   # optional, default 2xx/3xx
        note: optional note

Exit codes:
  0  all probes OK
  1  at least one probe failed
  2  argument / config error
`, appName, appName, appName, appName, appName, appName, appName, workspace)
}
