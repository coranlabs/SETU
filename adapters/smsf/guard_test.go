// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package smsf

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// loopback is the trusted list for the test servers (httptest listens on 127.0.0.1).
var loopback = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

func guardedSMSF(t *testing.T, trusted ...netip.Prefix) *SMSF {
	t.Helper()
	s, err := New(Config{APIRoot: "http://smsf.test", AMF: "http://127.0.0.1:1", Trusted: trusted}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Without OAuth the allowlist is all that stops a stranger sending sms as a subscriber: every API route refuses an
// untrusted caller before acting, and the UE's context is untouched.
func TestGuardRefusesUntrustedCallers(t *testing.T) {
	s := guardedSMSF(t, netip.MustParsePrefix("10.42.0.0/16"))
	h := s.Handler()
	const ctx = `{"supi":"imsi-001010100007488","amfId":"cafe00","accessType":"3GPP_ACCESS"}`
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/nsmsf-sms/v2/ue-contexts/imsi-001010100007488", ctx},
		{"POST", "/nsmsf-sms/v2/ue-contexts/imsi-001010100007488/sendsms", "x"},
		{"DELETE", "/nsmsf-sms/v2/ue-contexts/imsi-001010100007488", ""},
		{"POST", "/nsmsf-callback/v1/n1n2-failure/t1", "{}"},
		{"GET", "/healthz", ""}, // open to loopback only
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		r.RemoteAddr = "192.0.2.7:40000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s from 192.0.2.7: %d, want 403", c.method, c.path, w.Code)
		}
	}
	if _, ok := s.store.get("imsi-001010100007488"); ok {
		t.Fatal("an untrusted Activate created a context")
	}

	// the AMF's pod network is let through
	r := httptest.NewRequest("PUT", "/nsmsf-sms/v2/ue-contexts/imsi-001010100007488", strings.NewReader(ctx))
	r.RemoteAddr = "10.42.3.15:51000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("Activate from the AMF's pod network: %d, want 201", w.Code)
	}
	// an IPv4-mapped IPv6 caller is the same address
	r = httptest.NewRequest("DELETE", "/nsmsf-sms/v2/ue-contexts/imsi-001010100007488", nil)
	r.RemoteAddr = "[::ffff:10.42.3.15]:51000"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("Deactivate from ::ffff:10.42.3.15: %d, want 204", w.Code)
	}
}

func TestGuardHealthzOpenToLoopback(t *testing.T) {
	h := guardedSMSF(t, netip.MustParsePrefix("10.42.0.0/16")).Handler()
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("healthz from loopback: %d, want 200", w.Code)
	}
	// loopback reaches healthz only: the API itself still needs a trusted caller
	r = httptest.NewRequest("DELETE", "/nsmsf-sms/v2/ue-contexts/imsi-001010100007488", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("API from untrusted loopback: %d, want 403", w.Code)
	}
}

func TestGuardLogsEachRefusedAddressOnce(t *testing.T) {
	s := guardedSMSF(t, loopback...)
	a := netip.MustParseAddr("192.0.2.7")
	if !s.noteRefused(a) || s.noteRefused(a) {
		t.Fatal("the first refusal is logged, the second is not")
	}
	for i := range maxRefusedLogged {
		s.noteRefused(netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)}))
	}
	if s.noteRefused(netip.MustParseAddr("203.0.113.1")) {
		t.Fatal("past the cap a new address is still logged: a scan would flood the log and grow the set")
	}
}

func TestNewRequiresTrusted(t *testing.T) {
	if _, err := New(Config{APIRoot: "http://smsf.test", AMF: "http://127.0.0.1:1"}, nil); err == nil ||
		!strings.Contains(err.Error(), "trusted is required") {
		t.Fatalf("New without trusted = %v, want refused", err)
	}
}
