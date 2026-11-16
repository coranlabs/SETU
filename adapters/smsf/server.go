// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package smsf

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/coranlabs/SETU/ims/ident"
	"github.com/coranlabs/SETU/sms/cp"
	"github.com/coranlabs/SETU/uilog"
)

const maxBody = 64 << 10 // an SMS context or one CP message is far smaller

// ueSmsContextData is the part of TS 29.540's UeSmsContextData the SMSF uses; the rest is echoed, not read.
type ueSmsContextData struct {
	SUPI       string  `json:"supi"`
	AMFID      string  `json:"amfId"`
	AccessType string  `json:"accessType"`
	GPSI       string  `json:"gpsi"`
	GUAMIs     []guami `json:"guamis"`
}

// smsRecordData is the JSON part of an UplinkSMS (TS 29.540 SmsRecordData).
type smsRecordData struct {
	SMSRecordID string `json:"smsRecordId"`
	SMSPayload  struct {
		ContentID string `json:"contentId"`
	} `json:"smsPayload"`
}

// Handler is the Nsmsf_SMService API and the N1N2 failure callback.
func (s *SMSF) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /nsmsf-sms/v2/ue-contexts/{supi}", s.handleActivate)
	mux.HandleFunc("DELETE /nsmsf-sms/v2/ue-contexts/{supi}", s.handleDeactivate)
	mux.HandleFunc("POST /nsmsf-sms/v2/ue-contexts/{supi}/sendsms", s.handleSendSMS)
	mux.HandleFunc("POST /nsmsf-callback/v1/n1n2-failure/{txn}", s.handleFailure)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusNotFound, "RESOURCE_URI_STRUCTURE_NOT_FOUND", "no such resource")
	})
	return s.guard(mux)
}

// guard lets only callers in cfg.Trusted reach the API; /healthz stays open to loopback (the container healthcheck).
// A refused address is logged once, so a scanner cannot flood the log.
func (s *SMSF) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ap, err := netip.ParseAddrPort(r.RemoteAddr)
		ip := ap.Addr().Unmap()
		switch {
		case err != nil:
		case r.URL.Path == "/healthz" && ip.IsLoopback(), s.isTrusted(ip):
			next.ServeHTTP(w, r)
			return
		}
		if s.noteRefused(ip) {
			uilog.Event(uilog.Orange, "SMSF", "refused a call from %s: not in smsf.trusted", ip)
		}
		problem(w, http.StatusForbidden, "FORBIDDEN", "caller not trusted")
	})
}

func (s *SMSF) isTrusted(ip netip.Addr) bool {
	for _, p := range s.cfg.Trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// maxRefusedLogged bounds the set of refused addresses kept for the log-once rule.
const maxRefusedLogged = 256

// noteRefused says whether ip is refused for the first time (and should be logged).
func (s *SMSF) noteRefused(ip netip.Addr) bool {
	s.refusedMu.Lock()
	defer s.refusedMu.Unlock()
	if s.refused == nil {
		s.refused = map[netip.Addr]bool{}
	}
	if s.refused[ip] || len(s.refused) >= maxRefusedLogged {
		return false
	}
	s.refused[ip] = true
	return true
}

// Serve registers with the NRF (when there is one) and serves the API until it fails.
func (s *SMSF) Serve() error {
	if s.nrf != nil {
		s.nrf.start()
	}
	log.Printf("smsf: Nsmsf_SMService on %s, advertised as %s", s.cfg.Listen, s.cfg.APIRoot)
	// Every request is one small JSON body or one CP message from the AMF: a client that sends slower than this, or
	// holds a connection idle, is cut off rather than holding a goroutine and a socket.
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	return srv.ListenAndServe()
}

// problem answers with a TS 29.500 ProblemDetails.
func problem(w http.ResponseWriter, status int, cause, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "cause": cause, "detail": detail})
}

// handleActivate is Activate (TS 29.540 5.2.2.2): the AMF tells the SMSF a UE may use SMS over NAS.
func (s *SMSF) handleActivate(w http.ResponseWriter, r *http.Request) {
	supi := r.PathValue("supi")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		problem(w, http.StatusBadRequest, "INVALID_MSG_FORMAT", "body unreadable")
		return
	}
	var d ueSmsContextData
	if err := json.Unmarshal(body, &d); err != nil {
		problem(w, http.StatusBadRequest, "INVALID_MSG_FORMAT", "body is not UeSmsContextData JSON")
		return
	}
	switch {
	case d.SUPI != supi:
		problem(w, http.StatusBadRequest, "MANDATORY_IE_INCORRECT", "supi differs from the one in the path")
		return
	case d.AMFID == "":
		problem(w, http.StatusBadRequest, "MANDATORY_IE_MISSING", "amfId is missing")
		return
	case d.AccessType == "":
		problem(w, http.StatusBadRequest, "MANDATORY_IE_MISSING", "accessType is missing")
		return
	}
	c := ueCtx{SUPI: supi, GPSI: d.GPSI, MSISDN: s.msisdnOf(r.Context(), supi, d.GPSI), AMFID: d.AMFID,
		AccessType: d.AccessType, GUAMIs: d.GUAMIs, Updated: time.Now()}
	created := s.store.put(c)
	s.saveState()
	if c.MSISDN == "" {
		uilog.Event(uilog.Orange, "SMSF", "%s sms over nas on, but it has no number: it can neither send nor get sms", uilog.Mask(supi))
	} else {
		uilog.Event(uilog.Yellow, "SMSF", "%s sms over nas on (number %s, %s)", uilog.Mask(supi), uilog.Mask(c.MSISDN), d.AccessType)
	}
	if !created {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Location", s.cfg.APIRoot+"/nsmsf-sms/v2/ue-contexts/"+supi)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

// msisdnOf is the UE's number: the gpsi the AMF sent, or else the UDR's am-data.
func (s *SMSF) msisdnOf(ctx context.Context, supi, gpsi string) string {
	if m, err := ident.MSISDNFromGPSI(gpsi); err == nil {
		return m
	}
	if s.sub == nil || !strings.HasPrefix(supi, "imsi-") {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	m, err := s.sub.MSISDN(ctx, strings.TrimPrefix(supi, "imsi-"))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(m, "msisdn-"), "+")
}

// handleDeactivate is Deactivate (TS 29.540 5.2.2.3). MT messages waiting for the UE fail, so the router can try IMS.
func (s *SMSF) handleDeactivate(w http.ResponseWriter, r *http.Request) {
	supi := r.PathValue("supi")
	if _, ok := s.store.del(supi); !ok {
		problem(w, http.StatusNotFound, "CONTEXT_NOT_FOUND", "no sms context for this ue")
		return
	}
	s.stopWorker(supi)
	s.saveState()
	uilog.Event(uilog.Yellow, "SMSF", "%s sms over nas off", uilog.Mask(supi))
	w.WriteHeader(http.StatusNoContent)
}

// handleSendSMS is UplinkSMS (TS 29.540 5.2.2.4): one CP message from the phone. The answer is written before the
// message is acted on: the AMF waits for it while it handles the phone's UL NAS TRANSPORT, so an N1N2 sent first
// could meet an AMF still busy with this UE.
func (s *SMSF) handleSendSMS(w http.ResponseWriter, r *http.Request) {
	supi := r.PathValue("supi")
	if _, ok := s.store.get(supi); !ok {
		problem(w, http.StatusNotFound, "CONTEXT_NOT_FOUND", "no sms context for this ue")
		return
	}
	rec, payload := parseUplink(r.Header.Get("Content-Type"), http.MaxBytesReader(w, r.Body, maxBody))
	if len(payload) == 0 {
		problem(w, http.StatusBadRequest, "SMS_PAYLOAD_MISSING", "no binary part with the sms")
		return
	}
	m, err := cp.Decode(payload)
	if err != nil {
		problem(w, http.StatusBadRequest, "SMS_PAYLOAD_ERROR", "the sms payload is not a CP message")
		return
	}
	wk := s.workerFor(supi)
	if wk == nil {
		problem(w, http.StatusNotFound, "CONTEXT_NOT_FOUND", "no sms context for this ue")
		return
	}
	gate := make(chan struct{})
	defer close(gate)
	if !wk.tryPost(event{kind: evUplink, msg: m, gate: gate}) {
		problem(w, http.StatusServiceUnavailable, "INSUFFICIENT_RESOURCES", "too many sms waiting for this ue")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"smsRecordId": rec, "deliveryStatus": "SMS_DELIVERY_SMSF_ACCEPTED"})
	_ = http.NewResponseController(w).Flush()
}

// parseUplink finds the record id and the CP message in an UplinkSMS body (multipart/related). The JSON part is the
// one named jsonData, else the first JSON part; the binary part is the one its smsPayload.contentId names, with or
// without <>, else the first part that is not JSON.
func parseUplink(contentType string, body io.Reader) (recordID string, payload []byte) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" {
		return "", nil
	}
	type part struct {
		id, ctype string
		body      []byte
	}
	var parts []part
	mr := multipart.NewReader(body, params["boundary"])
	for {
		p, err := mr.NextRawPart()
		if err != nil {
			break // io.EOF, or a broken body: what was read so far still counts
		}
		b, err := io.ReadAll(p)
		if err != nil {
			break
		}
		parts = append(parts, part{id: strings.Trim(p.Header.Get("Content-ID"), "<> "), ctype: p.Header.Get("Content-Type"), body: b})
	}
	isJSON := func(ct string) bool {
		t, _, _ := mime.ParseMediaType(ct)
		return t == "application/json" || strings.HasSuffix(t, "+json")
	}
	var js *part
	for i := range parts {
		if parts[i].id == "jsonData" {
			js = &parts[i]
			break
		}
	}
	for i := range parts {
		if js == nil && isJSON(parts[i].ctype) {
			js = &parts[i]
		}
	}
	var rec smsRecordData
	if js != nil {
		_ = json.Unmarshal(js.body, &rec) // a broken JSON part still leaves the binary part to find
	}
	if cid := strings.Trim(rec.SMSPayload.ContentID, "<> "); cid != "" {
		for i := range parts {
			if &parts[i] != js && parts[i].id == cid {
				return rec.SMSRecordID, parts[i].body
			}
		}
	}
	for i := range parts {
		if &parts[i] != js && !isJSON(parts[i].ctype) {
			return rec.SMSRecordID, parts[i].body
		}
	}
	return rec.SMSRecordID, nil
}

// handleFailure is the AMF's N1N2 failure notification (TS 29.518 6.1.5.2): paging for an MT message failed.
func (s *SMSF) handleFailure(w http.ResponseWriter, r *http.Request) {
	txn := r.PathValue("txn")
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxBody))
	s.mu.Lock()
	wk := s.workers[s.txns[txn]]
	s.mu.Unlock()
	if wk != nil {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		wk.post(ctx, event{kind: evFailure, txn: txn})
		cancel()
	}
	w.WriteHeader(http.StatusNoContent)
}
