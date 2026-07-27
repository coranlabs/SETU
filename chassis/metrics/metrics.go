// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package metrics is a tiny, dependency-free Prometheus text-exposition registry
// (counters + gauges) plus a health handler, for the SD-Core bridges. Stdlib only,
// so it builds and tests offline.
package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Counter is a monotonically increasing value.
type Counter struct{ v uint64 }

func (c *Counter) Inc()          { atomic.AddUint64(&c.v, 1) }
func (c *Counter) Add(n uint64)  { atomic.AddUint64(&c.v, n) }
func (c *Counter) Value() uint64 { return atomic.LoadUint64(&c.v) }

// Gauge is a value that can go up and down.
type Gauge struct{ v int64 }

func (g *Gauge) Set(n int64)  { atomic.StoreInt64(&g.v, n) }
func (g *Gauge) Inc()         { atomic.AddInt64(&g.v, 1) }
func (g *Gauge) Dec()         { atomic.AddInt64(&g.v, -1) }
func (g *Gauge) Value() int64 { return atomic.LoadInt64(&g.v) }

type entry struct {
	name    string
	help    string
	typ     string // "counter" | "gauge"
	counter *Counter
	gauge   *Gauge
}

// Registry holds a set of named metrics.
type Registry struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// New creates an empty registry.
func New() *Registry { return &Registry{entries: map[string]*entry{}} }

// Counter registers (or returns) a counter with the given name and help text.
func (r *Registry) Counter(name, help string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[name]; ok {
		if e.counter == nil {
			panic("metrics: " + name + " already registered as a gauge")
		}
		return e.counter
	}
	c := &Counter{}
	r.entries[name] = &entry{name: name, help: help, typ: "counter", counter: c}
	return c
}

// Gauge registers (or returns) a gauge with the given name and help text.
func (r *Registry) Gauge(name, help string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[name]; ok {
		if e.gauge == nil {
			panic("metrics: " + name + " already registered as a counter")
		}
		return e.gauge
	}
	g := &Gauge{}
	r.entries[name] = &entry{name: name, help: help, typ: "gauge", gauge: g}
	return g
}

// WriteText renders the registry in Prometheus text-exposition format.
func (r *Registry) WriteText(w io.Writer) {
	r.mu.Lock()
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	es := r.entries
	r.mu.Unlock()
	sort.Strings(names)
	for _, n := range names {
		e := es[n]
		if e.help != "" {
			fmt.Fprintf(w, "# HELP %s %s\n", e.name, e.help)
		}
		fmt.Fprintf(w, "# TYPE %s %s\n", e.name, e.typ)
		if e.counter != nil {
			fmt.Fprintf(w, "%s %d\n", e.name, e.counter.Value())
		} else {
			fmt.Fprintf(w, "%s %d\n", e.name, e.gauge.Value())
		}
	}
}

// Handler serves the metrics in Prometheus text format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.WriteText(w)
	})
}

var start = time.Now()

// HealthHandler serves a liveness check as JSON (200 OK while the process runs).
func HealthHandler(service, version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"service": service,
			"version": version,
			"uptime":  time.Since(start).Round(time.Second).String(),
		})
	})
}

// Mux returns an http.ServeMux exposing /metrics and /healthz for the service.
func (r *Registry) Mux(service, version string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", r.Handler())
	mux.Handle("/healthz", HealthHandler(service, version))
	return mux
}
