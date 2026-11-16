// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package smsf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coranlabs/SETU/sms/cp"
	"github.com/coranlabs/SETU/sms/route"
	"github.com/coranlabs/SETU/sms/tpdu"
)

const (
	supiA, numA = "imsi-001010100007487", "0100007487"
	supiB, numB = "imsi-001010100007488", "0100007488"
	amfUUID     = "0f8e7d6c-1111-4222-8333-444455556666"
	tc1         = 150 * time.Millisecond
)

// ---- a fake AMF: records each N1N2MessageTransfer per UE and answers with what answer says ----

type mpart struct {
	ctype, id string
	body      []byte
}

type n1msg struct {
	ue, ctype string
	parts     []mpart
	req       n1n2Req
	raw       string // the JSON part as sent
	cp        []byte
	at        time.Time
}

type fakeAMF struct {
	srv    *httptest.Server
	mu     sync.Mutex
	chans  map[string]chan n1msg
	answer func(n1msg) int
	hold   chan struct{} // set: every request waits for it to close
}

func newFakeAMF(t *testing.T) *fakeAMF {
	f := &fakeAMF{chans: map[string]chan n1msg{}, answer: func(n1msg) int { return http.StatusOK }}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAMF) ch(ue string) chan n1msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.chans[ue]
	if c == nil {
		c = make(chan n1msg, 128)
		f.chans[ue] = c
	}
	return c
}

func (f *fakeAMF) set(answer func(n1msg) int) { f.mu.Lock(); f.answer = answer; f.mu.Unlock() }

func (f *fakeAMF) setHold(h chan struct{}) { f.mu.Lock(); f.hold = h; f.mu.Unlock() }

func (f *fakeAMF) serve(w http.ResponseWriter, r *http.Request) {
	ue := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/namf-comm/v1/ue-contexts/"), "/n1-n2-messages")
	m := n1msg{ue: ue, ctype: r.Header.Get("Content-Type"), at: time.Now()}
	if _, params, err := mime.ParseMediaType(m.ctype); err == nil {
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			m.parts = append(m.parts, mpart{p.Header.Get("Content-Type"), p.Header.Get("Content-ID"), b})
		}
	}
	if len(m.parts) == 2 {
		m.raw = string(m.parts[0].body)
		_ = json.Unmarshal(m.parts[0].body, &m.req)
		m.cp = m.parts[1].body
	}
	f.ch(ue) <- m
	f.mu.Lock()
	hold, answer := f.hold, f.answer
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	w.WriteHeader(answer(m))
}

func (f *fakeAMF) next(t *testing.T, ue string) n1msg {
	t.Helper()
	select {
	case m := <-f.ch(ue):
		return m
	case <-time.After(3 * time.Second):
		t.Fatalf("no N1N2 for %s", ue)
		return n1msg{}
	}
}

func (f *fakeAMF) none(t *testing.T, ue string, d time.Duration) {
	t.Helper()
	select {
	case m := <-f.ch(ue):
		t.Fatalf("unexpected N1N2 for %s: % x", ue, m.cp)
	case <-time.After(d):
	}
}

// ---- the SMSF under test ----

func newTestSMSF(t *testing.T, amf string, rt Router, mod func(*Config)) (*SMSF, *httptest.Server) {
	t.Helper()
	cfg := Config{Trusted: loopback, APIRoot: "http://smsf.test:29540", AMF: amf, TC1: tc1, MaxRetrans: 2, PagingWait: 300 * time.Millisecond}
	if mod != nil {
		mod(&cfg)
	}
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.retryGap = 30 * time.Millisecond
	s.Router = rt
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		stopAll(s)
		srv.Close()
	})
	return s, srv
}

func stopAll(s *SMSF) {
	s.mu.Lock()
	var ws []*worker
	for _, w := range s.workers {
		ws = append(ws, w)
	}
	s.mu.Unlock()
	for _, w := range ws {
		w.stop()
	}
}

func do(t *testing.T, method, url, ctype, body string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func activateWith(t *testing.T, srv *httptest.Server, supi, gpsi, amfID string) (int, http.Header, []byte) {
	t.Helper()
	body := `{"supi":"` + supi + `","amfId":"` + amfID + `","accessType":"3GPP_ACCESS","pei":"imeisv-1234567890123456"`
	if gpsi != "" {
		body += `,"gpsi":"` + gpsi + `"`
	}
	body += `,"guamis":[{"plmnId":{"mcc":"001","mnc":"01"},"amfId":"cafe00"}]}`
	return do(t, http.MethodPut, srv.URL+"/nsmsf-sms/v2/ue-contexts/"+supi, "application/json", body)
}

func activate(t *testing.T, srv *httptest.Server, supi, num string) {
	t.Helper()
	if st, _, b := activateWith(t, srv, supi, "msisdn-"+num, amfUUID); st != http.StatusCreated && st != http.StatusNoContent {
		t.Fatalf("activate %s: %d %s", supi, st, b)
	}
}

// sendsms posts one CP message the way the TossiCore AMF does: jsonData, then the binary part binaryPayload.
func sendsms(t *testing.T, srv *httptest.Server, supi string, msg []byte) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range []struct{ ct, id, body string }{
		{"application/json", "jsonData", `{"smsRecordId":"rec-1","smsPayload":{"contentId":"binaryPayload"},"accessType":"3GPP_ACCESS","gpsi":"msisdn-x"}`},
		{"application/vnd.3gpp.sms", "binaryPayload", string(msg)},
	} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", p.ct)
		h.Set("Content-ID", p.id)
		pw, _ := mw.CreatePart(h)
		_, _ = pw.Write([]byte(p.body))
	}
	mw.Close()
	ct := `multipart/related; boundary=` + mw.Boundary() + `; type="application/json"`
	st, _, b := do(t, http.MethodPost, srv.URL+"/nsmsf-sms/v2/ue-contexts/"+supi+"/sendsms", ct, buf.String())
	return st, b
}

// ue sends msg as the phone and expects the SMSF to take it.
func ue(t *testing.T, srv *httptest.Server, supi string, msg []byte) {
	t.Helper()
	if st, b := sendsms(t, srv, supi, msg); st != http.StatusOK {
		t.Fatalf("sendsms % x: %d %s", msg, st, b)
	}
}

// moRP is a phone's RP-DATA: "Hi" to the number, with RP-MR ref.
func moRP(to string, ref byte) []byte {
	ud, septets := tpdu.GSM7Pack("Hi")
	submit := []byte{0x01, 0x00, byte(len(to)), 0x91}
	submit = append(submit, tpdu.EncodeBCDAddr(to)...)
	submit = append(submit, 0x00, 0x00, byte(septets))
	submit = append(submit, ud...)
	return append([]byte{0x00, ref, 0x00, 0x02, 0x91, 0xf7, byte(len(submit))}, submit...)
}

// fakeRouter records each MO and answers out, after hold closes when set.
type fakeRouter struct {
	mu    sync.Mutex
	calls []string // sender>recipient
	out   route.Outcome
	hold  chan struct{}
}

func (r *fakeRouter) Route(_ context.Context, sender string, mo *tpdu.MO) route.Outcome {
	r.mu.Lock()
	r.calls = append(r.calls, sender+">"+mo.Recipient)
	hold := r.hold
	r.mu.Unlock()
	if hold != nil {
		<-hold
	}
	return r.out
}

func (r *fakeRouter) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func problemCause(t *testing.T, h http.Header, b []byte) string {
	t.Helper()
	if ct := h.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("problem Content-Type %q", ct)
	}
	var p struct {
		Status int    `json:"status"`
		Cause  string `json:"cause"`
	}
	_ = json.Unmarshal(b, &p)
	return p.Cause
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

// ---- Activate / Deactivate ----

func TestActivate(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	st, h, b := activateWith(t, srv, supiA, "msisdn-"+numA, amfUUID)
	if st != http.StatusCreated {
		t.Fatalf("new context: %d %s", st, b)
	}
	if loc := h.Get("Location"); loc != "http://smsf.test:29540/nsmsf-sms/v2/ue-contexts/"+supiA {
		t.Fatalf("Location %q", loc)
	}
	var echo map[string]any
	if json.Unmarshal(b, &echo) != nil || echo["supi"] != supiA || echo["pei"] != "imeisv-1234567890123456" {
		t.Fatalf("body not echoed: %s", b)
	}
	if !s.Reachable(numA) {
		t.Fatal("number not reachable after activate")
	}
	if st, _, _ := activateWith(t, srv, supiA, "msisdn-"+numA, amfUUID); st != http.StatusNoContent {
		t.Fatalf("updated context: %d, want 204", st)
	}

	url := srv.URL + "/nsmsf-sms/v2/ue-contexts/" + supiA
	for _, c := range []struct{ name, body, cause string }{
		{"supi differs", `{"supi":"` + supiB + `","amfId":"x","accessType":"3GPP_ACCESS"}`, "MANDATORY_IE_INCORRECT"},
		{"no supi", `{"amfId":"x","accessType":"3GPP_ACCESS"}`, "MANDATORY_IE_INCORRECT"},
		{"no amfId", `{"supi":"` + supiA + `","accessType":"3GPP_ACCESS"}`, "MANDATORY_IE_MISSING"},
		{"no accessType", `{"supi":"` + supiA + `","amfId":"x"}`, "MANDATORY_IE_MISSING"},
		{"not JSON", `{"supi":`, "INVALID_MSG_FORMAT"},
	} {
		st, h, b := do(t, http.MethodPut, url, "application/json", c.body)
		if st != http.StatusBadRequest || problemCause(t, h, b) != c.cause {
			t.Errorf("%s: %d %s, want 400 %s", c.name, st, b, c.cause)
		}
	}
}

type fakeSub struct{ imsi, msisdn string }

func (f fakeSub) Exists(context.Context, string) (bool, error) { return true, nil }
func (f fakeSub) MSISDN(_ context.Context, imsi string) (string, error) {
	if imsi == f.imsi {
		return f.msisdn, nil
	}
	return "", errors.New("not found")
}

// without a gpsi the number comes from the subscriber data
func TestActivateLooksUpTheNumber(t *testing.T) {
	amf := newFakeAMF(t)
	s, err := New(Config{Trusted: loopback, APIRoot: "http://smsf.test", AMF: amf.srv.URL}, fakeSub{"001010100007487", "0100007499"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if st, _, b := activateWith(t, srv, supiA, "", amfUUID); st != http.StatusCreated {
		t.Fatalf("%d %s", st, b)
	}
	if !s.Reachable("0100007499") {
		t.Fatal("the looked-up number is not reachable")
	}
	// no gpsi and no subscriber data: the context exists without a number
	if st, _, _ := activateWith(t, srv, supiB, "", amfUUID); st != http.StatusCreated {
		t.Fatal("context without a number refused")
	}
	if c, _ := s.store.get(supiB); c.MSISDN != "" {
		t.Fatalf("number %q from nowhere", c.MSISDN)
	}
}

func TestDeactivate(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	activate(t, srv, supiA, numA)
	url := srv.URL + "/nsmsf-sms/v2/ue-contexts/" + supiA
	if st, _, b := do(t, http.MethodDelete, url, "", ""); st != http.StatusNoContent {
		t.Fatalf("deactivate: %d %s", st, b)
	}
	if s.Reachable(numA) {
		t.Fatal("reachable after deactivate")
	}
	st, h, b := do(t, http.MethodDelete, url, "", "")
	if st != http.StatusNotFound || problemCause(t, h, b) != "CONTEXT_NOT_FOUND" {
		t.Fatalf("second deactivate: %d %s", st, b)
	}
}

// a context deactivated and activated again at once works with a new worker, not the one still shutting down
func TestReactivateRightAway(t *testing.T) {
	amf := newFakeAMF(t)
	_, srv := newTestSMSF(t, amf.srv.URL, &fakeRouter{out: route.Outcome{Delivered: true}}, nil)
	activate(t, srv, supiA, numA)
	ue(t, srv, supiA, cp.EncodeAck(false, 3)) // starts the worker
	do(t, http.MethodDelete, srv.URL+"/nsmsf-sms/v2/ue-contexts/"+supiA, "", "")
	activate(t, srv, supiA, numA)
	ue(t, srv, supiA, cp.EncodeData(false, 0, moRP(numB, 1)))
	if m := amf.next(t, supiA); !bytes.Equal(m.cp, []byte{0x89, 0x04}) {
		t.Fatalf("% x, want the CP-ACK", m.cp)
	}
}

// a number two SIMs share is not routed over NAS until only one has it
func TestSharedNumber(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	activate(t, srv, supiA, "0100007400")
	activate(t, srv, supiB, "0100007400")
	if s.Reachable("0100007400") {
		t.Fatal("a shared number is reachable")
	}
	if a := s.Deliver(context.Background(), "0100007400", []byte{0x01, 0x00}); a != route.Failed(27) {
		t.Fatalf("delivery to a shared number: %+v", a)
	}
	amf.none(t, supiA, 50*time.Millisecond)
	amf.none(t, supiB, 0)
	do(t, http.MethodDelete, srv.URL+"/nsmsf-sms/v2/ue-contexts/"+supiB, "", "")
	if !s.Reachable("0100007400") {
		t.Fatal("not reachable once only one SIM has the number")
	}
	// an SMS context for something other than an IMSI is never routed to
	activate(t, srv, "nai-sim@example.org", "0100007401")
	if s.Reachable("0100007401") {
		t.Fatal("a non-IMSI SUPI is reachable")
	}
}

// ---- UplinkSMS ----

func TestParseUplink(t *testing.T) {
	build := func(parts ...mpart) (string, *bytes.Buffer) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, p := range parts {
			h := textproto.MIMEHeader{}
			h.Set("Content-Type", p.ctype)
			if p.id != "" {
				h.Set("Content-ID", p.id)
			}
			pw, _ := mw.CreatePart(h)
			_, _ = pw.Write(p.body)
		}
		mw.Close()
		return `multipart/related; boundary=` + mw.Boundary() + `; type="application/json"`, &buf
	}
	payload := []byte{0x09, 0x04}
	decoy := []byte{0xde, 0xad}
	for _, c := range []struct {
		name  string
		parts []mpart
		rec   string
		want  []byte
	}{
		{"TossiCore AMF style", []mpart{
			{"application/json", "jsonData", []byte(`{"smsRecordId":"r1","smsPayload":{"contentId":"binaryPayload"}}`)},
			{"application/vnd.3gpp.sms", "binaryPayload", payload}}, "r1", payload},
		{"spec style, <cid> on the part", []mpart{
			{"application/json", "<root@amf>", []byte(`{"smsRecordId":"r2","smsPayload":{"contentId":"sms"}}`)},
			{"application/octet-stream", "<other>", decoy},
			{"application/vnd.3gpp.sms", "<sms>", payload}}, "r2", payload},
		{"spec style, <cid> in the JSON", []mpart{
			{"application/json; charset=utf-8", "", []byte(`{"smsRecordId":"r3","smsPayload":{"contentId":"<sms>"}}`)},
			{"application/octet-stream", "other", decoy},
			{"application/vnd.3gpp.sms", "sms", payload}}, "r3", payload},
		{"no contentId: the first non-JSON part", []mpart{
			{"application/json", "jsonData", []byte(`{"smsRecordId":"r4"}`)},
			{"application/vnd.3gpp.sms", "x", payload}}, "r4", payload},
		{"only JSON", []mpart{{"application/json", "jsonData", []byte(`{"smsRecordId":"r5"}`)}}, "r5", nil},
	} {
		ct, body := build(c.parts...)
		rec, got := parseUplink(ct, body)
		if rec != c.rec || !bytes.Equal(got, c.want) {
			t.Errorf("%s: %q % x, want %q % x", c.name, rec, got, c.rec, c.want)
		}
	}
	if _, got := parseUplink("application/json", strings.NewReader(`{}`)); got != nil {
		t.Fatal("a plain JSON body gave a payload")
	}
}

func TestSendSMSAnswers(t *testing.T) {
	amf := newFakeAMF(t)
	_, srv := newTestSMSF(t, amf.srv.URL, &fakeRouter{}, nil)
	activate(t, srv, supiA, numA)
	st, b := sendsms(t, srv, supiA, cp.EncodeAck(false, 3)) // a CP-ACK for nothing: taken and ignored
	var got map[string]string
	if st != http.StatusOK || json.Unmarshal(b, &got) != nil || got["smsRecordId"] != "rec-1" ||
		got["deliveryStatus"] != "SMS_DELIVERY_SMSF_ACCEPTED" {
		t.Fatalf("answer %d %s", st, b)
	}
	url := srv.URL + "/nsmsf-sms/v2/ue-contexts/"
	related := `multipart/related; boundary=B; type="application/json"`
	jsonPart := "--B\r\nContent-Type: application/json\r\nContent-ID: jsonData\r\n\r\n{\"smsRecordId\":\"r\",\"smsPayload\":{\"contentId\":\"binaryPayload\"}}\r\n"
	for _, c := range []struct {
		name, supi, ctype, body, cause string
		status                         int
	}{
		{"unknown UE", supiB, related, jsonPart + "--B\r\nContent-Type: application/vnd.3gpp.sms\r\nContent-ID: binaryPayload\r\n\r\n\x29\x04\r\n--B--\r\n", "CONTEXT_NOT_FOUND", 404},
		{"plain JSON", supiA, "application/json", `{"smsRecordId":"r"}`, "SMS_PAYLOAD_MISSING", 400},
		{"no binary part", supiA, related, jsonPart + "--B--\r\n", "SMS_PAYLOAD_MISSING", 400},
		{"empty binary part", supiA, related, jsonPart + "--B\r\nContent-Type: application/vnd.3gpp.sms\r\nContent-ID: binaryPayload\r\n\r\n\r\n--B--\r\n", "SMS_PAYLOAD_MISSING", 400},
	} {
		st, h, b := do(t, http.MethodPost, url+c.supi+"/sendsms", c.ctype, c.body)
		if st != c.status || problemCause(t, h, b) != c.cause {
			t.Errorf("%s: %d %s, want %d %s", c.name, st, b, c.status, c.cause)
		}
	}
	if st, b := sendsms(t, srv, supiA, []byte{0x7e, 0x00, 0x41}); st != 400 || !strings.Contains(string(b), "SMS_PAYLOAD_ERROR") {
		t.Fatalf("a 5GMM message as the payload: %d %s", st, b)
	}
}

// the sendsms answer goes out while the CP-ACK's N1N2 is still waiting at the AMF
func TestSendSMSAnswersBeforeTheN1N2(t *testing.T) {
	amf := newFakeAMF(t)
	_, srv := newTestSMSF(t, amf.srv.URL, &fakeRouter{out: route.Outcome{Delivered: true}}, nil)
	activate(t, srv, supiA, numA)
	hold := make(chan struct{})
	amf.setHold(hold)
	defer close(hold)
	done := make(chan int, 1)
	go func() { st, _ := sendsms(t, srv, supiA, cp.EncodeData(false, 0, moRP(numB, 9))); done <- st }()
	select {
	case st := <-done:
		if st != http.StatusOK {
			t.Fatalf("sendsms %d", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendsms waited for the N1N2")
	}
	if m := amf.next(t, supiA); !bytes.Equal(m.cp, []byte{0x89, 0x04}) {
		t.Fatalf("first N1N2 % x, want the CP-ACK 89 04", m.cp)
	}
}

// the worker does not act on a CP message until its sendsms answer is out
func TestUplinkWaitsForTheGate(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, &fakeRouter{}, nil)
	activate(t, srv, supiA, numA)
	w := s.workerFor(supiA)
	gate := make(chan struct{})
	w.post(context.Background(), event{kind: evUplink, msg: cp.Msg{Type: cp.TypeData, TI: 4, RPDU: []byte{tpdu.RPSMMA, 0x01}}, gate: gate})
	amf.none(t, supiA, 150*time.Millisecond)
	close(gate)
	if m := amf.next(t, supiA); !bytes.Equal(m.cp, []byte{0xc9, 0x04}) {
		t.Fatalf("N1N2 % x, want the CP-ACK c9 04", m.cp)
	}
}

func TestSendSMSQueueFull(t *testing.T) {
	amf := newFakeAMF(t)
	_, srv := newTestSMSF(t, amf.srv.URL, &fakeRouter{}, nil)
	activate(t, srv, supiA, numA)
	hold := make(chan struct{})
	amf.setHold(hold)
	defer close(hold)
	for i := range 2 * inbox {
		st, b := sendsms(t, srv, supiA, cp.EncodeData(false, uint8(i%7), moRP(numB, byte(i))))
		if st == http.StatusServiceUnavailable {
			if !strings.Contains(string(b), "INSUFFICIENT_RESOURCES") {
				t.Fatalf("503 body %s", b)
			}
			return
		}
		if st != http.StatusOK {
			t.Fatalf("sendsms %d: %d %s", i, st, b)
		}
	}
	t.Fatal("never 503 with the worker stuck")
}

func TestHealthAndUnknownPaths(t *testing.T) {
	amf := newFakeAMF(t)
	_, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	if st, _, b := do(t, http.MethodGet, srv.URL+"/healthz", "", ""); st != 200 || string(b) != "ok" {
		t.Fatalf("healthz %d %s", st, b)
	}
	st, h, b := do(t, http.MethodGet, srv.URL+"/nsmsf-sms/v1/whatever", "", "")
	if st != 404 || problemCause(t, h, b) == "" {
		t.Fatalf("unknown path %d %s", st, b)
	}
}
