// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package sessionstore

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func rec(loc, tok string) Record {
	return Record{Location: loc, Token: tok, LastSeen: time.Unix(1_700_000_000, 0).UTC()}
}

// Put/Delete/Load round-trip, then reopen and prove the state survived a
// "restart" — the crash-recovery guarantee the gateway depends on.
func TestFileStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.wal")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("sid-A", rec("http://pcf/app-sessions/A", "tokA")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("sid-B", rec("http://pcf/app-sessions/B", "tokB")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("sid-A"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// reopen — simulates a gateway restart
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after reopen: %d records, want 1 (A deleted, B kept)", len(got))
	}
	b, ok := got["sid-B"]
	if !ok || b.Location != "http://pcf/app-sessions/B" || b.Token != "tokB" {
		t.Fatalf("sid-B did not survive: %+v", got)
	}
	if _, gone := got["sid-A"]; gone {
		t.Fatal("sid-A should have been deleted before restart")
	}
}

// After reopen the WAL must be compacted to one line per live session, so the
// file cannot grow without bound across a long-lived process's churn.
func TestFileStoreCompaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.wal")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// churn: many puts+dels leaving a single live session
	for i := 0; i < 500; i++ {
		id := "sid-" + itoa(i)
		if err := s.Put(id, rec("http://pcf/x/"+id, "t")); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put("survivor", rec("http://pcf/x/survivor", "t")); err != nil {
		t.Fatal(err)
	}
	sizeBefore := fileSize(t, path)
	s.Close()

	s2, err := Open(path) // compacts on open
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	sizeAfter := fileSize(t, path)
	if sizeAfter >= sizeBefore {
		t.Fatalf("compaction did not shrink WAL: before=%d after=%d", sizeBefore, sizeAfter)
	}
	got, _ := s2.Load()
	if len(got) != 1 {
		t.Fatalf("post-compaction live set = %d, want 1", len(got))
	}
}

// A crash mid-write can leave a truncated/garbage trailing line. Replay must keep
// every complete record before it and not fail the whole recovery.
func TestFileStoreToleratesTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.wal")
	s, _ := Open(path)
	_ = s.Put("sid-good", rec("http://pcf/good", "g"))
	s.Close()

	// append a torn line, as a crash would
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"op":"put","sid":"sid-torn","rec":{"loc":"http://pc`) // no newline, truncated JSON
	f.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("recovery failed on truncated tail: %v", err)
	}
	defer s2.Close()
	got, _ := s2.Load()
	if _, ok := got["sid-good"]; !ok {
		t.Fatal("good record lost during truncated-tail recovery")
	}
	if _, ok := got["sid-torn"]; ok {
		t.Fatal("torn record should not have been recovered")
	}
}

// Concurrent Put/Delete must be race-clean (run under -race).
func TestFileStoreConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.wal")
	s, _ := Open(path)
	s.Sync = false // faster; durability semantics unchanged for this race check
	defer s.Close()

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				id := "sid-" + itoa(base*1000+i)
				_ = s.Put(id, rec("http://pcf/x/"+id, "t"))
				if i%2 == 0 {
					_ = s.Delete(id)
				}
				_, _ = s.Load()
			}
		}(w)
	}
	wg.Wait()
	got, _ := s.Load()
	if len(got) != 400 { // 8 workers × 50 odd-index survivors
		t.Fatalf("concurrent live set = %d, want 400", len(got))
	}
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
