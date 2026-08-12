// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package sessionstore

import (
	"path/filepath"
	"testing"
)

// A second process (simulated: second Open in-process holds a distinct fd) must
// fail fast rather than share the WAL — shared appends were observed to lose
// tombstones and resurrect grants.
func TestSecondOpenFailsFast(t *testing.T) {
	p := filepath.Join(t.TempDir(), "grants.wal")
	s1, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	if _, err := Open(p); err == nil {
		t.Fatal("second Open must fail while the first holds the lock")
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(p)
	if err != nil {
		t.Fatalf("Open after Close must succeed: %v", err)
	}
	s3.Close()
}
