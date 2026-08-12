// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"path/filepath"
	"testing"

	api "github.com/coranlabs/SETU/api/v1"
)

// TestGrantsSurviveRestart is THE fabric characterization test: the exact defect
// the original gateway had (in-memory session map, WAL unwired, restart leaks the
// PCF app-session) must be impossible here.
func TestGrantsSurviveRestart(t *testing.T) {
	wal := filepath.Join(t.TempDir(), "grants.wal")

	g1, restored, err := Open(wal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 {
		t.Fatalf("fresh WAL restored %d grants, want 0", len(restored))
	}
	loc := api.CoreRef("https://pcf:29507/npcf-policyauthorization/v1/app-sessions/imsi-001010000000001-24")
	if err := g1.Bind("rx-sess-1", loc); err != nil {
		t.Fatal(err)
	}
	if err := g1.Bind("rx-sess-2", "https://pcf:29507/npcf-policyauthorization/v1/app-sessions/imsi-001010000000002-27"); err != nil {
		t.Fatal(err)
	}
	if _, ok := g1.Take("rx-sess-2"); !ok { // normal STR teardown before the crash
		t.Fatal("Take(rx-sess-2) failed")
	}
	_ = g1.Close() // simulated crash/restart boundary

	g2, restored, err := Open(wal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 {
		t.Fatalf("restored %d grants after restart, want 1 (only the live one)", len(restored))
	}
	ref, ok := g2.Take("rx-sess-1")
	if !ok || ref != loc {
		t.Fatalf("restored grant = %q ok=%v, want %q", ref, ok, loc)
	}
	if _, ok := g2.Take("rx-sess-2"); ok {
		t.Fatal("deleted grant resurrected after restart")
	}
}

func TestMemoryOnlyMode(t *testing.T) {
	g, restored, err := Open("")
	if err != nil || restored != nil {
		t.Fatalf("Open(\"\") = restored %v err %v", restored, err)
	}
	_ = g.Bind("s", "ref")
	if ref, ok := g.Take("s"); !ok || ref != "ref" {
		t.Fatal("memory-only bind/take failed")
	}
}
