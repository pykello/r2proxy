package main

import (
	"math/rand"
	"path"
	"strings"
	"sync"
	"time"
)

// Rule is a single error/latency injection rule. Filters use glob patterns
// (path.Match syntax: * ? [..]). An empty or "*" filter matches anything.
type Rule struct {
	ID          string  `json:"id"`
	Enabled     bool    `json:"enabled"`
	Key         string  `json:"key"`         // glob, "" or "*" = any
	Op          string  `json:"op"`          // op name(s), comma-separated; "" = any
	Probability float64 `json:"probability"` // 0..1 chance the rule fires per matching request
	Status      int     `json:"status"`      // HTTP status to inject (0 = no error, delay only)
	Code        string  `json:"code"`        // S3 error code, e.g. ServiceUnavailable
	Message     string  `json:"message"`     // S3 error message
	RetryAfter  int     `json:"retry_after"` // Retry-After header seconds (0 = R2 default for 429/503)
	DelayMs     int     `json:"delay_ms"`    // latency injected before responding
	// MaxFailuresPerObject caps how many times this rule injects an error for any
	// single object (bucket/key); once reached, that object passes through. 0 =
	// unlimited. Use with probability=1 for a deterministic "fail N times then
	// recover" retry test.
	MaxFailuresPerObject int   `json:"max_failures_per_object"`
	Hits                 int64 `json:"hits"` // times this rule fired

	// runtime, not persisted: injected-failure count per "bucket/key".
	failed map[string]int
}

// capReached reports whether an object has already hit this rule's per-object
// failure cap. A nil map reads as 0, so this is safe before any failures.
func (r *Rule) capReached(obj string) bool {
	return r.MaxFailuresPerObject > 0 && r.failed[obj] >= r.MaxFailuresPerObject
}

func (r *Rule) recordFailure(obj string) {
	if r.failed == nil {
		r.failed = map[string]int{}
	}
	r.failed[obj]++
}

// Decision is the outcome of consulting the injection engine for a request.
type Decision struct {
	Inject     bool // short-circuit with an error response
	Status     int
	Code       string
	Message    string
	Delay      time.Duration
	RetryAfter int
	RuleID     string
}

// Engine evaluates the ordered rule list against requests.
type Engine struct {
	mu    sync.Mutex
	rules []*Rule
	rng   *rand.Rand
}

func newEngine(rules []*Rule) *Engine {
	if rules == nil {
		rules = []*Rule{}
	}
	return &Engine{
		rules: rules,
		rng:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// decide walks the rules in order. The first enabled rule that matches the
// request's filters and wins its probability roll determines the outcome.
// A rule with Status==0 injects only latency and does not short-circuit;
// evaluation then continues to later rules.
func (e *Engine) decide(op, bucket, key string) Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	obj := bucket + "/" + key
	var d Decision
	for _, r := range e.rules {
		if !r.Enabled {
			continue
		}
		if !r.matches(op, key) {
			continue
		}
		// Once an object has hit this rule's per-object failure cap, the rule is
		// inert for it: the request passes through (or a later rule may apply).
		if r.Status > 0 && r.capReached(obj) {
			continue
		}
		if r.Probability < 1.0 && e.rng.Float64() >= r.Probability {
			continue
		}
		// Rule fires.
		r.Hits++
		if r.DelayMs > 0 {
			d.Delay += time.Duration(r.DelayMs) * time.Millisecond
		}
		if r.Status > 0 {
			r.recordFailure(obj) // count only injected failures toward the cap
			d.Inject = true
			d.Status = r.Status
			d.Code = r.Code
			d.Message = r.Message
			d.RetryAfter = r.RetryAfter
			d.RuleID = r.ID
			return d
		}
		// Latency-only rule: keep evaluating subsequent rules.
	}
	return d
}

func (r *Rule) matches(op, key string) bool {
	return globMatch(r.Key, key) && opMatch(r.Op, op)
}

func globMatch(pattern, s string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	ok, err := path.Match(pattern, s)
	if err != nil {
		return pattern == s
	}
	return ok
}

func opMatch(pattern, op string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	for _, want := range strings.Split(pattern, ",") {
		want = strings.TrimSpace(want)
		if want == "" || strings.EqualFold(want, op) {
			return true
		}
	}
	return false
}

// list returns a copy of the current rules for display.
func (e *Engine) list() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Rule, len(e.rules))
	for i, r := range e.rules {
		out[i] = *r
	}
	return out
}

func (e *Engine) add(r *Rule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = append(e.rules, r)
}

func (e *Engine) remove(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, r := range e.rules {
		if r.ID == id {
			e.rules = append(e.rules[:i], e.rules[i+1:]...)
			return true
		}
	}
	return false
}

func (e *Engine) toggle(id string) (bool, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.rules {
		if r.ID == id {
			r.Enabled = !r.Enabled
			return r.Enabled, true
		}
	}
	return false, false
}

func (e *Engine) clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = e.rules[:0]
}

// rulesSnapshot returns the underlying slice for persistence (caller holds no
// lock; used under manager save which copies).
func (e *Engine) rulesSnapshot() []*Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Rule, len(e.rules))
	copy(out, e.rules)
	return out
}

// errorTemplates are convenient presets exposed to the UI/CLI. The 429 preset
// is verified byte-for-byte against a real R2 same-object throttle response.
var errorTemplates = []Rule{
	{Code: "ServiceUnavailable", Status: 429, Message: "Reduce your concurrent request rate for the same object.", RetryAfter: 5},
	{Code: "SlowDown", Status: 503, Message: "Please reduce your request rate.", RetryAfter: 1},
	{Code: "InternalError", Status: 500, Message: "We encountered an internal error. Please try again."},
}
