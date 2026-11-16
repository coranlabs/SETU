// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package uilog

import "testing"

// safeMask calls Mask and turns a panic into a value, so one crashing case is reported and the others still run.
func safeMask(s string) (out string, panicked any) {
	defer func() { panicked = recover() }()
	out = Mask(s)
	return out, nil
}

// Mask keeps only the last 4 characters of a subscriber identity (phone number, IMSI) behind a fixed "***", hides
// short ones completely, and never panics: a log line must not crash the server.
func TestMask(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"9000007487", "***7487"},      // phone number
		{"001010100007487", "***7487"}, // IMSI
		{"+919876543210", "***3210"},   // the + is hidden with the rest
		{"12345", "***2345"},           // 5 characters: the shortest that keeps 4
		{"7487", "***"},                // 4 or fewer: showing 4 would show all of it
		{"748", "***"},
		{"", "-"}, // empty: the line still shows something here
	} {
		got, panicked := safeMask(c.in)
		switch {
		case panicked != nil:
			t.Errorf("Mask(%q) panicked: %v (want %q)", c.in, panicked, c.want)
		case got != c.want:
			t.Errorf("Mask(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShortMasksTheSubscriberInARef(t *testing.T) {
	got := Short("https://pcf:29507/npcf-policyauthorization/v1/app-sessions/imsi-001010100007488-1")
	if got != "imsi-***7488-1" {
		t.Fatalf("Short = %q, want imsi-***7488-1", got)
	}
	if Short("sess-call") != "sess-call" {
		t.Fatal("a ref without a subscriber changed")
	}
}
