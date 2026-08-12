// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package uilog

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func capture(t *testing.T, f func()) string {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(nil); log.SetFlags(log.LstdFlags) })
	f()
	return buf.String()
}

func TestEventLine(t *testing.T) {
	out := capture(t, func() {
		Event(Green, "🎧", "AUDIO", "ue %s  sess %s", "198.51.100.89", "imsi-1-24")
	})
	for _, want := range []string{"🎧", "AUDIO", "198.51.100.89", "imsi-1-24", Green, reset} {
		if !strings.Contains(out, want) {
			t.Errorf("line missing %q\ngot: %q", want, out)
		}
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("one event should print one line, got %d", n)
	}
	// the timestamp we render ourselves, so the stdlib prefix must stay off
	if strings.Count(out, ":") < 2 {
		t.Errorf("expected an HH:MM:SS timestamp, got %q", out)
	}
}

func TestShort(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://pcf:29507/npcf-policyauthorization/v1/app-sessions/imsi-001-24", "imsi-001-24"},
		{"imsi-001-24", "imsi-001-24"},
		{"", ""},
		{"trailing/", "trailing/"}, // nothing after the separator: keep it whole
	} {
		if got := Short(tc.in); got != tc.want {
			t.Errorf("Short(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Short is generic so it accepts named string types such as api.CoreRef.
func TestShortNamedType(t *testing.T) {
	type ref string
	if got := Short(ref("a/b/c")); got != "c" {
		t.Errorf("got %q, want c", got)
	}
}
