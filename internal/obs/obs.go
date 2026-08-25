// Package obs exposes process-wide counters, a Prometheus-style text
// rendering, and a tiny health struct used by daemon and CLI alike.
package obs

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// CounterMap is a concurrency-safe integer counter set.
type CounterMap struct {
	mu sync.Mutex
	m  map[string]int64
}

// NewCounterMap returns an empty counter map.
func NewCounterMap() *CounterMap { return &CounterMap{m: map[string]int64{}} }

// Inc increments a counter (creates it when absent).
func (c *CounterMap) Inc(name string, delta int64) { c.Add(name, delta) }

// Add applies a delta (positive or negative).
func (c *CounterMap) Add(name string, delta int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[name] += delta
}

// Get returns the current value of a counter (0 when absent).
func (c *CounterMap) Get(name string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[name]
}

// Snapshot returns a sorted copy of the map.
func (c *CounterMap) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

// MetricsText renders counters in Prometheus text exposition format:
// "<name> <value>\n", name-sorted.
func (c *CounterMap) MetricsText() string {
	snap := c.Snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %d\n", k, snap[k])
	}
	return b.String()
}

// Health is a point-in-time liveness statement.
type Health struct {
	Status    string `json:"status"`
	CheckedAt int64  `json:"checked_at"`
}

// HealthNow builds a Health record.
func HealthNow(ok bool) Health {
	s := "ok"
	if !ok {
		s = "degraded"
	}
	return Health{Status: s, CheckedAt: time.Now().Unix()}
}
