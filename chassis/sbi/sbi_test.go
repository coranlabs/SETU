// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package sbi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The TokenSource must obtain a client_credentials token from the NRF with the right
// form fields, cache it, and refresh only when it nears expiry.
func TestTokenFetchAndCache(t *testing.T) {
	var issued int64
	nrf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/token" || r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("targetNfType") != "PCF" ||
			r.Form.Get("scope") != "npcf-policyauthorization" || r.Form.Get("nfType") != "AF" {
			t.Errorf("bad token form: %v", r.Form)
		}
		n := atomic.AddInt64(&issued, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok-` + itoa(n) + `","token_type":"Bearer","expires_in":3600,"scope":"npcf-policyauthorization"}`))
	}))
	defer nrf.Close()

	base := time.Unix(1_700_000_000, 0)
	ts := &TokenSource{
		NRFTokenURL: nrf.URL + "/oauth2/token", NFInstanceID: "af-uuid", NFType: "AF",
		TargetNFType: "PCF", Scope: "npcf-policyauthorization", HTTP: nrf.Client(),
		Now: func() time.Time { return base },
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok-1" {
		t.Fatalf("token = %q, want tok-1", tok)
	}
	// cached: no new fetch
	if tok2, _ := ts.Token(); tok2 != "tok-1" {
		t.Fatalf("second token = %q, want cached tok-1", tok2)
	}
	if atomic.LoadInt64(&issued) != 1 {
		t.Fatalf("NRF hit %d times, want 1 (not cached)", issued)
	}
	// jump past expiry -> refresh
	ts.Now = func() time.Time { return base.Add(2 * time.Hour) }
	if tok3, _ := ts.Token(); tok3 != "tok-2" {
		t.Fatalf("refreshed token = %q, want tok-2", tok3)
	}
}

func TestTokenErrors(t *testing.T) {
	nrf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer nrf.Close()
	ts := &TokenSource{NRFTokenURL: nrf.URL + "/oauth2/token", HTTP: nrf.Client()}
	if _, err := ts.Token(); err == nil {
		t.Fatal("expected error on 403 from NRF")
	}
}

// Decorate must add the 3gpp-Sbi-* headers and the Bearer token.
func TestDecorate(t *testing.T) {
	nrf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"abc","token_type":"Bearer","expires_in":3600}`))
	}))
	defer nrf.Close()
	c := &Client{Token: &TokenSource{NRFTokenURL: nrf.URL + "/oauth2/token", TargetNFType: "PCF", HTTP: nrf.Client()}}

	req, _ := http.NewRequest(http.MethodPost, "http://pcf:29507/npcf-policyauthorization/v1/app-sessions", nil)
	if err := c.Decorate(req, APIRoot("http://pcf:29507/npcf-policyauthorization/v1")); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer abc" {
		t.Fatalf("Authorization = %q, want Bearer abc", got)
	}
	if got := req.Header.Get("3gpp-Sbi-Target-apiRoot"); got != "http://pcf:29507" {
		t.Fatalf("Target-apiRoot = %q", got)
	}
	if req.Header.Get("3gpp-Sbi-Sender-Timestamp") == "" {
		t.Fatal("missing 3gpp-Sbi-Sender-Timestamp")
	}

	// nil client / nil token: headers still applied, no Authorization, no panic
	var nilc *Client
	req2, _ := http.NewRequest(http.MethodPost, "http://pcf/x", nil)
	if err := nilc.Decorate(req2, "http://pcf"); err != nil {
		t.Fatal(err)
	}
	if req2.Header.Get("Authorization") != "" {
		t.Fatal("Authorization set without a TokenSource")
	}
}

func TestAPIRoot(t *testing.T) {
	if got := APIRoot("https://pcf.5gc:29507/npcf-policyauthorization/v1/app-sessions"); got != "https://pcf.5gc:29507" {
		t.Fatalf("APIRoot = %q", got)
	}
	if APIRoot("not a url") != "" {
		t.Fatal("APIRoot should be empty for a bad URL")
	}
	_ = url.Values{}
	_ = strings.TrimSpace
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
