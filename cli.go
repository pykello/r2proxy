package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// adminClient talks to the admin API for the CLI subcommands.
type adminClient struct {
	base  string
	token string
}

func newAdminClient() *adminClient {
	base := envOr("R2PROXY_ADMIN", "http://127.0.0.1:8081")
	return &adminClient{
		base:  strings.TrimRight(base, "/"),
		token: os.Getenv("R2PROXY_TOKEN"),
	}
}

func (c *adminClient) do(method, path string, body any) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

func (c *adminClient) mustJSON(method, path string, body any, out any) {
	data, status, err := c.do(method, path, body)
	if err != nil {
		fatal("request failed: %v (is the proxy running? set R2PROXY_ADMIN)", err)
	}
	if status >= 300 {
		fatal("admin API error %d: %s", status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			fatal("bad response: %v\n%s", err, data)
		}
	}
}

// ---- subcommand: stats ----

func cmdStats(args []string) {
	fs := newFlagSet("stats")
	asJSON := fs.Bool("json", false, "print raw JSON")
	parseCLIFlags(fs, args)
	c := newAdminClient()
	var snap StatsSnapshot
	c.mustJSON("GET", "/api/stats", nil, &snap)
	if *asJSON {
		printJSON(snap)
		return
	}
	fmt.Printf("uptime   %.1f min\n", snap.UptimeSec/60)
	fmt.Printf("requests %d   (%.2f/s)   in-flight %d\n", snap.Total, snap.ReqPerSec, snap.InFlight)
	fmt.Printf("injected %d   errors %d\n", snap.Injected, snap.Errors)
	fmt.Printf("bytes    in %s  out %s\n", humanBytes(snap.BytesIn), humanBytes(snap.BytesOut))
	fmt.Printf("latency  p50 %.0f  p90 %.0f  p99 %.0f  avg %.1f ms\n",
		snap.LatencyMs["p50"], snap.LatencyMs["p90"], snap.LatencyMs["p99"], snap.LatencyMs["avg"])
	if len(snap.ByOp) > 0 {
		fmt.Println("\nby op:")
		type kv struct {
			k string
			v *OpStat
		}
		var ops []kv
		for k, v := range snap.ByOp {
			ops = append(ops, kv{k, v})
		}
		sort.Slice(ops, func(i, j int) bool { return ops[i].v.Count > ops[j].v.Count })
		for _, o := range ops {
			fmt.Printf("  %-24s %6d  err %-4d inj %-4d\n", o.k, o.v.Count, o.v.Errors, o.v.Injected)
		}
	}
}

// ---- subcommand: tail ----

func cmdTail(args []string) {
	c := newAdminClient()
	seen := map[string]bool{}
	fmt.Printf("%-12s %-22s %-18s %-6s %8s\n", "time", "op", "bucket/key", "status", "ms")
	for {
		var rows []reqRecord
		c.mustJSON("GET", "/api/recent", nil, &rows)
		// rows are newest-first; print oldest new ones first
		for i := len(rows) - 1; i >= 0; i-- {
			r := rows[i]
			key := r.Time.Format(time.RFC3339Nano) + r.Op + r.Key + strconv.Itoa(r.Status)
			if seen[key] {
				continue
			}
			seen[key] = true
			bk := r.Bucket
			if r.Key != "" {
				bk += "/" + r.Key
			}
			inj := ""
			if r.Injected {
				inj = " [inj]"
			}
			fmt.Printf("%-12s %-22s %-18s %-6d %8.1f%s\n",
				r.Time.Format("15:04:05.000"), r.Op, truncate(bk, 18), r.Status, r.DurationMs, inj)
		}
		if len(seen) > 5000 {
			seen = map[string]bool{}
		}
		time.Sleep(1 * time.Second)
	}
}

// ---- subcommand: rules ----

func cmdRules(args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	c := newAdminClient()
	switch args[0] {
	case "list":
		var rules []Rule
		c.mustJSON("GET", "/api/rules", nil, &rules)
		if len(rules) == 0 {
			fmt.Println("(no rules)")
			return
		}
		fmt.Printf("%-14s %-4s %-22s %-10s %-5s %-9s %-7s %s\n", "id", "on", "op", "key", "prob", "status", "delay", "hits")
		for _, r := range rules {
			on := "off"
			if r.Enabled {
				on = "on"
			}
			fmt.Printf("%-14s %-4s %-22s %-10s %-5.2g %-3d %-5s %-7d %d\n",
				r.ID, on, orStar(r.Op), orStar(r.Key), r.Probability, r.Status, r.Code, r.DelayMs, r.Hits)
		}
	case "add":
		fs := newFlagSet("rules add")
		op := fs.String("op", "", "op filter (csv)")
		key := fs.String("key", "", "key glob")
		prob := fs.Float64("prob", 1.0, "probability 0..1")
		status := fs.Int("status", 503, "http status")
		code := fs.String("code", "", "S3 error code (blank = R2 default for status)")
		msg := fs.String("message", "", "error message (blank = R2 default)")
		retryAfter := fs.Int("retry-after", 0, "Retry-After seconds (0 = R2 default for 429/503)")
		delay := fs.Int("delay", 0, "delay ms")
		parseCLIFlags(fs, args[1:])
		rule := Rule{Op: *op, Key: *key, Probability: *prob,
			Status: *status, Code: *code, Message: *msg, RetryAfter: *retryAfter, DelayMs: *delay}
		var out Rule
		c.mustJSON("POST", "/api/rules", rule, &out)
		fmt.Printf("added rule %s\n", out.ID)
	case "rm":
		if len(args) < 2 {
			fatal("usage: r2proxy rules rm <id>")
		}
		c.mustJSON("DELETE", "/api/rules/"+args[1], nil, nil)
		fmt.Println("deleted", args[1])
	case "toggle":
		if len(args) < 2 {
			fatal("usage: r2proxy rules toggle <id>")
		}
		c.mustJSON("POST", "/api/rules/"+args[1]+"/toggle", nil, nil)
		fmt.Println("toggled", args[1])
	case "clear":
		c.mustJSON("DELETE", "/api/rules", nil, nil)
		fmt.Println("cleared")
	default:
		fatal("unknown rules subcommand %q (list|add|rm|toggle|clear)", args[0])
	}
}

// ---- helpers ----

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func orStar(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func humanBytes(n int64) string {
	f := float64(n)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
