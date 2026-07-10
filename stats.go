package main

import (
	"sort"
	"sync"
	"time"
)

// OpStat aggregates counters for one grouping key (an op, a bucket, ...).
type OpStat struct {
	Count    int64   `json:"count"`
	Errors   int64   `json:"errors"`   // responses with status >= 400
	Injected int64   `json:"injected"` // injected error responses
	BytesIn  int64   `json:"bytes_in"`
	BytesOut int64   `json:"bytes_out"`
	LatSumMs float64 `json:"lat_sum_ms"`
	LatMaxMs float64 `json:"lat_max_ms"`
}

func (o *OpStat) add(s reqRecord) {
	o.Count++
	if s.Injected {
		o.Injected++
	}
	if s.Status >= 400 {
		o.Errors++
	}
	o.BytesIn += s.BytesIn
	o.BytesOut += s.BytesOut
	o.LatSumMs += s.DurationMs
	if s.DurationMs > o.LatMaxMs {
		o.LatMaxMs = s.DurationMs
	}
}

// reqRecord is a single completed request, kept in a ring buffer for the UI.
type reqRecord struct {
	Time       time.Time `json:"time"`
	Method     string    `json:"method"`
	Op         string    `json:"op"`
	Bucket     string    `json:"bucket"`
	Key        string    `json:"key"`
	Status     int       `json:"status"`
	DurationMs float64   `json:"duration_ms"`
	BytesIn    int64     `json:"bytes_in"`
	BytesOut   int64     `json:"bytes_out"`
	Injected   bool      `json:"injected"`
	Remote     string    `json:"remote"`
	Err        string    `json:"err,omitempty"`
}

// Stats is a concurrency-safe collector for one tenant.
type Stats struct {
	mu        sync.Mutex
	start     time.Time
	total     int64
	injected  int64
	errors    int64
	bytesIn   int64
	bytesOut  int64
	inFlight  int64
	byOp      map[string]*OpStat
	byBucket  map[string]*OpStat
	byStatus  map[int]int64
	latencies []float64 // ring buffer of recent latencies for percentiles
	latPos    int
	recent    []reqRecord // ring buffer of recent requests
	recentPos int
	seq       int64
}

const (
	maxLatencies = 4096
	maxRecent    = 200
)

func newStats() *Stats {
	return &Stats{
		start:    time.Now(),
		byOp:     map[string]*OpStat{},
		byBucket: map[string]*OpStat{},
		byStatus: map[int]int64{},
	}
}

func (s *Stats) begin() {
	s.mu.Lock()
	s.inFlight++
	s.mu.Unlock()
}

func (s *Stats) record(r reqRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight > 0 {
		s.inFlight--
	}
	s.total++
	s.bytesIn += r.BytesIn
	s.bytesOut += r.BytesOut
	if r.Injected {
		s.injected++
	}
	if r.Status >= 400 {
		s.errors++
	}
	s.byStatus[r.Status]++
	s.opStat(s.byOp, r.Op).add(r)
	if r.Bucket != "" {
		s.opStat(s.byBucket, r.Bucket).add(r)
	}

	if len(s.latencies) < maxLatencies {
		s.latencies = append(s.latencies, r.DurationMs)
	} else {
		s.latencies[s.latPos] = r.DurationMs
		s.latPos = (s.latPos + 1) % maxLatencies
	}

	if len(s.recent) < maxRecent {
		s.recent = append(s.recent, r)
	} else {
		s.recent[s.recentPos] = r
		s.recentPos = (s.recentPos + 1) % maxRecent
	}
	s.seq++
}

func (s *Stats) opStat(m map[string]*OpStat, k string) *OpStat {
	o := m[k]
	if o == nil {
		o = &OpStat{}
		m[k] = o
	}
	return o
}

func (s *Stats) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	inFlight := s.inFlight
	*s = Stats{
		start:    time.Now(),
		byOp:     map[string]*OpStat{},
		byBucket: map[string]*OpStat{},
		byStatus: map[int]int64{},
		inFlight: inFlight,
	}
}

// StatsSnapshot is the JSON shape returned by the admin API.
type StatsSnapshot struct {
	UptimeSec float64            `json:"uptime_sec"`
	Total     int64              `json:"total"`
	Injected  int64              `json:"injected"`
	Errors    int64              `json:"errors"`
	InFlight  int64              `json:"in_flight"`
	BytesIn   int64              `json:"bytes_in"`
	BytesOut  int64              `json:"bytes_out"`
	ReqPerSec float64            `json:"req_per_sec"`
	LatencyMs map[string]float64 `json:"latency_ms"`
	ByOp      map[string]*OpStat `json:"by_op"`
	ByBucket  map[string]*OpStat `json:"by_bucket"`
	ByStatus  map[string]int64   `json:"by_status"`
}

func (s *Stats) snapshot() StatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	uptime := time.Since(s.start).Seconds()
	rps := 0.0
	if uptime > 0 {
		rps = float64(s.total) / uptime
	}
	byStatus := make(map[string]int64, len(s.byStatus))
	for code, n := range s.byStatus {
		byStatus[itoa(code)] = n
	}
	return StatsSnapshot{
		UptimeSec: uptime,
		Total:     s.total,
		Injected:  s.injected,
		Errors:    s.errors,
		InFlight:  s.inFlight,
		BytesIn:   s.bytesIn,
		BytesOut:  s.bytesOut,
		ReqPerSec: rps,
		LatencyMs: percentiles(s.latencies),
		ByOp:      cloneOps(s.byOp),
		ByBucket:  cloneOps(s.byBucket),
		ByStatus:  byStatus,
	}
}

func (s *Stats) recentCopy() []reqRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Return newest-first.
	out := make([]reqRecord, 0, len(s.recent))
	if len(s.recent) < maxRecent {
		for i := len(s.recent) - 1; i >= 0; i-- {
			out = append(out, s.recent[i])
		}
		return out
	}
	for i := 0; i < maxRecent; i++ {
		idx := (s.recentPos - 1 - i + maxRecent) % maxRecent
		out = append(out, s.recent[idx])
	}
	return out
}

func cloneOps(m map[string]*OpStat) map[string]*OpStat {
	out := make(map[string]*OpStat, len(m))
	for k, v := range m {
		cp := *v
		out[k] = &cp
	}
	return out
}

func percentiles(in []float64) map[string]float64 {
	out := map[string]float64{"p50": 0, "p90": 0, "p99": 0, "avg": 0}
	if len(in) == 0 {
		return out
	}
	cp := make([]float64, len(in))
	copy(cp, in)
	sort.Float64s(cp)
	var sum float64
	for _, v := range cp {
		sum += v
	}
	out["avg"] = sum / float64(len(cp))
	out["p50"] = pct(cp, 0.50)
	out["p90"] = pct(cp, 0.90)
	out["p99"] = pct(cp, 0.99)
	return out
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
