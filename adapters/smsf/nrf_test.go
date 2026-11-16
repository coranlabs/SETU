// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package smsf

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coranlabs/SETU/sms/route"
)

// ---- the state file ----

func TestStateFile(t *testing.T) {
	amf := newFakeAMF(t)
	file := filepath.Join(t.TempDir(), "smsf.json")
	s1, srv := newTestSMSF(t, amf.srv.URL, nil, func(c *Config) { c.StateFile = file })
	activate(t, srv, supiA, numA)
	activate(t, srv, supiB, numB)
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file %v, %v: want it written, owner-only", fi, err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Trusted: loopback, APIRoot: "http://smsf.test", AMF: amf.srv.URL, StateFile: file}
	s2, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Reachable(numA) || !s2.Reachable(numB) {
		t.Fatal("contexts not restored")
	}
	if c, _ := s2.store.get(supiA); c.AMFID != amfUUID || c.AccessType != "3GPP_ACCESS" || len(c.GUAMIs) != 1 {
		t.Fatalf("restored as %+v", c)
	}

	// an old context is dropped at start
	b, _ := os.ReadFile(file)
	var all []ueCtx
	if err := json.Unmarshal(b, &all); err != nil || len(all) != 2 {
		t.Fatalf("file %s: %v", b, err)
	}
	all[0].Updated = time.Now().Add(-25 * time.Hour)
	b, _ = json.Marshal(all)
	_ = os.WriteFile(file, b, 0o600)
	s3, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s3.Reachable(all[0].MSISDN) || !s3.Reachable(all[1].MSISDN) {
		t.Fatal("the old context was kept or the fresh one dropped")
	}

	// deactivate writes the file too
	do(t, http.MethodDelete, srv.URL+"/nsmsf-sms/v2/ue-contexts/"+supiB, "", "")
	b, _ = os.ReadFile(file)
	if strings.Contains(string(b), supiB) || !strings.Contains(string(b), supiA) {
		t.Fatalf("after deactivate: %s", b)
	}
	// a broken file stops the start instead of silently losing every context
	_ = os.WriteFile(file, []byte("{"), 0o600)
	if _, err := New(cfg, nil); err == nil {
		t.Fatal("a broken state file was accepted")
	}
}

// ---- a fake NRF ----

type fakeNRF struct {
	srv     *httptest.Server
	mu      sync.Mutex
	puts    [][]byte
	paths   []string
	patches []string
	deletes int
	discs   []url.Values
	forget  bool   // answer the next heartbeat 404
	amf     string // the namf-comm apiRoot of AMF amf-1
}

func newFakeNRF(t *testing.T, amfRoot string) *fakeNRF {
	f := &fakeNRF{amf: amfRoot}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNRF) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/nnrf-nfm/v1/nf-instances/"):
		f.puts = append(f.puts, b)
		f.paths = append(f.paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"nfStatus":"REGISTERED","heartBeatTimer":1}`))
	case r.Method == http.MethodPatch:
		f.patches = append(f.patches, r.Header.Get("Content-Type")+" "+string(b))
		if f.forget {
			f.forget = false
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		f.deletes++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/nnrf-disc/v1/nf-instances":
		q := r.URL.Query()
		f.discs = append(f.discs, q)
		w.Header().Set("Content-Type", "application/json")
		if q.Get("target-nf-instance-id") != "amf-1" || f.amf == "" {
			_, _ = w.Write([]byte(`{"validityPeriod":60,"nfInstances":[]}`))
			return
		}
		u, _ := url.Parse(f.amf)
		port, _ := strconv.Atoi(u.Port())
		_ = json.NewEncoder(w).Encode(map[string]any{"nfInstances": []any{map[string]any{
			"nfInstanceId": "amf-1", "nfType": "AMF", "nfStatus": "REGISTERED",
			"nfServices": []any{
				map[string]any{"serviceName": "namf-evts", "scheme": "http", "ipEndPoints": []any{map[string]any{"ipv4Address": "192.0.2.99", "port": 1}}},
				map[string]any{"serviceName": "namf-comm", "scheme": u.Scheme, "ipEndPoints": []any{map[string]any{"ipv4Address": u.Hostname(), "port": port}}},
			}}}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeNRF) counts() (puts, patches, deletes, discs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts), len(f.patches), f.deletes, len(f.discs)
}

const smsfUUID = "5b9f0c2e-7a1d-4e3f-9c8b-1a2b3c4d5e6f"

// the SMSF registers, heartbeats, registers again when the NRF has lost it, and deregisters on Close
func TestNRFRegistration(t *testing.T) {
	nrf := newFakeNRF(t, "")
	s, err := New(Config{Trusted: loopback, APIRoot: "http://192.0.2.10:29540", NRF: nrf.srv.URL, NFInstanceID: smsfUUID,
		PLMN: []PLMN{{MCC: "001", MNC: "01"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.nrf.hb = 40 * time.Millisecond
	s.nrf.start()
	eventually(t, "a registration and two heartbeats", func() bool { p, h, _, _ := nrf.counts(); return p >= 1 && h >= 2 })

	nrf.mu.Lock()
	path, put, patch := nrf.paths[0], nrf.puts[0], nrf.patches[0]
	nrf.mu.Unlock()
	if path != "/nnrf-nfm/v1/nf-instances/"+smsfUUID {
		t.Fatalf("PUT %s", path)
	}
	var p registerProfile
	if err := json.Unmarshal(put, &p); err != nil {
		t.Fatal(err)
	}
	if p.NFInstanceID != smsfUUID || p.NFType != "SMSF" || p.NFStatus != "REGISTERED" || len(p.PLMNList) != 1 ||
		p.PLMNList[0].MCC != "001" || len(p.IPv4Addresses) != 1 || p.IPv4Addresses[0] != "192.0.2.10" || len(p.NFServices) != 1 {
		t.Fatalf("profile %s", put)
	}
	svc := p.NFServices[0]
	if svc.ServiceInstanceID != "nsmsf-sms" || svc.ServiceName != "nsmsf-sms" || len(svc.Versions) != 1 ||
		svc.Versions[0].APIVersionInURI != "v2" || svc.Scheme != "http" || svc.NFServiceStatus != "REGISTERED" ||
		svc.APIPrefix != "http://192.0.2.10:29540" || len(svc.IPEndPoints) != 1 || svc.IPEndPoints[0].Port != 29540 ||
		svc.IPEndPoints[0].IPv4Address != "192.0.2.10" {
		t.Fatalf("service %s", put)
	}
	if patch != `application/json-patch+json [{"op":"replace","path":"/nfStatus","value":"REGISTERED"}]` {
		t.Fatalf("heartbeat %q", patch)
	}

	nrf.mu.Lock()
	nrf.forget = true
	nrf.mu.Unlock()
	eventually(t, "a second registration after the 404", func() bool { p, _, _, _ := nrf.counts(); return p >= 2 })

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, d, _ := nrf.counts(); d != 1 {
		t.Fatalf("%d DELETEs, want 1", d)
	}
}

// the AMF serving a UE is found by its NF instance id, once; an AMF the NRF does not know falls back to smsf.amf
func TestAMFLookup(t *testing.T) {
	amf1, amf2 := newFakeAMF(t), newFakeAMF(t)
	nrf := newFakeNRF(t, amf1.srv.URL)
	s, srv := newTestSMSF(t, amf2.srv.URL, nil, func(c *Config) { c.NRF, c.NFInstanceID = nrf.srv.URL, smsfUUID })
	ctx := context.Background()
	for range 2 {
		if r, err := s.amf.root(ctx, "amf-1"); err != nil || r != amf1.srv.URL {
			t.Fatalf("root(amf-1) = %q, %v, want %s", r, err, amf1.srv.URL)
		}
	}
	if _, _, _, n := nrf.counts(); n != 1 {
		t.Fatalf("%d discoveries, want 1 (cached)", n)
	}
	nrf.mu.Lock()
	q := nrf.discs[0]
	nrf.mu.Unlock()
	if q.Get("target-nf-type") != "AMF" || q.Get("requester-nf-type") != "SMSF" || q.Get("target-nf-instance-id") != "amf-1" {
		t.Fatalf("discovery query %v", q)
	}
	if r, err := s.amf.root(ctx, "amf-9"); err != nil || r != amf2.srv.URL {
		t.Fatalf("root(amf-9) = %q, %v, want the configured %s", r, err, amf2.srv.URL)
	}
	if _, err := (&amfClient{hc: http.DefaultClient, nrf: nrf.srv.URL, cache: map[string]string{}}).root(ctx, "amf-9"); err == nil {
		t.Fatal("no AMF found and no fallback, yet no error")
	}

	// end to end: an MT for a UE served by amf-1 goes there
	if st, _, b := activateWith(t, srv, supiB, "msisdn-"+numB, "amf-1"); st != http.StatusCreated {
		t.Fatalf("%d %s", st, b)
	}
	ch := deliver(s, numB, mtRP(1), 2*time.Second)
	amf1.next(t, supiB)
	amf2.none(t, supiB, 50*time.Millisecond)
	ue(t, srv, supiB, cpAck1(0))
	ue(t, srv, supiB, []byte{0x89, 0x01, 0x02, 0x02, 0x01})
	if a := result(t, ch); a != route.Delivered {
		t.Fatalf("result %+v", a)
	}
}

func cpAck1(ti uint8) []byte { return []byte{0x80 | ti<<4 | 0x09, 0x04} }

func TestCommRoot(t *testing.T) {
	for _, c := range []struct {
		name string
		p    nfProfile
		want string
	}{
		{"apiPrefix wins", nfProfile{NFServices: []nfService{{ServiceName: "namf-comm", APIPrefix: "https://amf.example:8443/"}}}, "https://amf.example:8443"},
		{"ip endpoint", nfProfile{NFServices: []nfService{{ServiceName: "namf-comm", Scheme: "http", IPEndPoints: []ipEndPoint{{IPv4Address: "10.0.0.5", Port: 29518}}}}}, "http://10.0.0.5:29518"},
		{"profile address, no port", nfProfile{IPv4Addresses: []string{"10.0.0.6"}, NFServices: []nfService{{ServiceName: "namf-comm", Scheme: "https"}}}, "https://10.0.0.6"},
		{"service map form", nfProfile{FQDN: "amf.local", NFServiceList: map[string]nfService{"x": {ServiceName: "namf-comm", Scheme: "http"}}}, "http://amf.local"},
		{"no namf-comm", nfProfile{NFServices: []nfService{{ServiceName: "namf-evts", APIPrefix: "http://x"}}}, ""},
	} {
		if got := commRoot(c.p); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// the client's errors name no subscriber: the request URL carries the SUPI
func TestTransferErrorHidesTheSUPI(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	c := &amfClient{hc: &http.Client{Timeout: time.Second}, static: "http://" + addr, cache: map[string]string{}}
	_, err := c.transfer(context.Background(), "", supiA, []byte{0x09, 0x04}, false, "", "")
	if err == nil || strings.Contains(err.Error(), "0100007487") || strings.Contains(err.Error(), "imsi") {
		t.Fatalf("error %v", err)
	}
}
