// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package udr

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A temporary IMPU (sip:<IMSI>@domain) must resolve to the subscriber's GPSI via a
// real UDR am-data lookup — NOT be mistaken for an MSISDN.
func TestGPSIResolverIMSI(t *testing.T) {
	var gotPath string
	udr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if strings.Contains(r.URL.Path, "imsi-001010000000001") && strings.HasSuffix(r.URL.Path, "am-data") {
			w.Write([]byte(`{"gpsis":["msisdn-9000000001"]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer udr.Close()

	resolve := GPSIResolver(udr.URL, "00101", udr.Client())
	gpsi, err := resolve("sip:001010000000001@ims.mnc001.mcc001.3gppnetwork.org")
	if err != nil {
		t.Fatal(err)
	}
	if gpsi != "msisdn-9000000001" {
		t.Fatalf("temp-IMPU GPSI = %q, want msisdn-9000000001 (IMSI wrongly treated as MSISDN?)", gpsi)
	}
	if !strings.Contains(gotPath, "imsi-001010000000001") {
		t.Fatalf("resolver did not query UDR am-data by IMSI: %q", gotPath)
	}
}

// An MSISDN IMPU resolves directly without a UDR call.
func TestGPSIResolverMSISDN(t *testing.T) {
	called := false
	udr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(404) }))
	defer udr.Close()
	resolve := GPSIResolver(udr.URL, "00101", udr.Client())
	gpsi, err := resolve("sip:9000000001@ims.mnc001.mcc001.3gppnetwork.org")
	if err != nil {
		t.Fatal(err)
	}
	if gpsi != "msisdn-9000000001" {
		t.Fatalf("MSISDN GPSI = %q", gpsi)
	}
	if called {
		t.Fatal("resolver hit UDR for a plain MSISDN (should short-circuit)")
	}
}

// An alphanumeric IMPU with no numeric identity must error (no GPSI => no binding).
func TestGPSIResolverAlnumErrors(t *testing.T) {
	resolve := GPSIResolver("http://udr", "00101", http.DefaultClient)
	if _, err := resolve("sip:alice@example.org"); err == nil {
		t.Fatal("expected error for alphanumeric IMPU with no numeric identity")
	}
}
