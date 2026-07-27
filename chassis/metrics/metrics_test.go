// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCounterAndGaugeExposition(t *testing.T) {
	r := New()
	c := r.Counter("aar_total", "AA-Requests received")
	g := r.Gauge("active_sessions", "active Rx sessions")
	c.Inc()
	c.Add(2)
	g.Inc()
	g.Inc()
	g.Dec()

	var sb strings.Builder
	r.WriteText(&sb)
	out := sb.String()

	for _, want := range []string{
		"# HELP aar_total AA-Requests received",
		"# TYPE aar_total counter",
		"aar_total 3",
		"# TYPE active_sessions gauge",
		"active_sessions 1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing %q in:\n%s", want, out)
		}
	}
}

// The same metric name must return the same underlying counter (idempotent register).
func TestCounterIdempotent(t *testing.T) {
	r := New()
	a := r.Counter("x_total", "x")
	b := r.Counter("x_total", "x")
	a.Inc()
	if b.Value() != 1 {
		t.Fatalf("counters not shared: b=%d", b.Value())
	}
}

func TestRegisterTypeConflictPanics(t *testing.T) {
	r := New()
	r.Counter("dup", "")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on type conflict")
		}
	}()
	r.Gauge("dup", "")
}

func TestMetricsHandler(t *testing.T) {
	r := New()
	r.Counter("hits_total", "hits").Add(5)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "hits_total 5") {
		t.Fatalf("body missing hits_total 5: %s", buf[:n])
	}
}

func TestHealthHandler(t *testing.T) {
	srv := httptest.NewServer(HealthHandler("rx-n5-gateway", "test"))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["service"] != "rx-n5-gateway" {
		t.Fatalf("health body = %+v", body)
	}
}
