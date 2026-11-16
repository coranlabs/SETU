// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package smsf is setu's SMSF: SMS over NAS for phones that send SMS on the 5G control channel instead of IMS
// (TS 23.502 4.13.3). The AMF activates a UE's SMS context here (Nsmsf_SMService, TS 29.540, nsmsf-sms v2) and
// forwards the phone's CP messages (sendsms); the SMSF runs the CP layer of TS 24.011 per UE, hands each RP-DATA to
// the shared router (sms/route) and sends its CP-ACKs, RP reports and MT messages back with Namf_Communication
// N1N2MessageTransfer (TS 29.518). It is also the router's NAS domain: a number with an SMS context gets its MT
// messages over NAS. Stdlib only, like the rest of setu.
package smsf

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	api "github.com/coranlabs/SETU/api/v1"
	"github.com/coranlabs/SETU/chassis/sbi"
	"github.com/coranlabs/SETU/sms/cp"
	"github.com/coranlabs/SETU/sms/route"
	"github.com/coranlabs/SETU/sms/tpdu"
)

// PLMN is one entry of the NRF profile's plmnList.
type PLMN struct {
	MCC string `json:"mcc"`
	MNC string `json:"mnc"`
}

// Config is everything the SMSF needs. Zero durations and counts take the defaults noted.
type Config struct {
	Listen        string           // ":29540"
	APIRoot       string           // how the AMF reaches this SMSF: Location headers, the failure callback, the NRF profile
	NFInstanceID  string           // this SMSF's NF instance id (a UUID); required with NRF
	NRF           string           // NRF apiRoot: register there and find each UE's AMF by its id; "" = neither
	AMF           string           // the AMF's apiRoot without an NRF, and the fallback when the lookup fails
	Insecure      bool             // self-signed SBI certificates (lab)
	StateFile     string           // "" = the SMS contexts live in memory only
	ContextMaxAge time.Duration    // a saved context older than this is dropped at start (24 h)
	TC1           time.Duration    // TC1*, the CP retransmission timer (6 s)
	MaxRetrans    int              // CP-DATA retransmissions on TC1* (2)
	PagingWait    time.Duration    // how long a 202 (the AMF is paging) waits before TC1* handling resumes (12 s)
	PLMN          []PLMN           // the NRF profile's plmnList
	Token         *sbi.TokenSource // OAuth for the AMF requests; nil = none (the TossiCore contract has no OAuth)
	// Trusted are the addresses allowed to call the API: the AMF's (its pod network, or its node when traffic is
	// masqueraded). Required: Nsmsf has no OAuth here, so an open port would let anyone send SMS as any subscriber.
	Trusted []netip.Prefix
}

// Router delivers an MO message and says what to tell the sender (route.Router).
type Router interface {
	Route(ctx context.Context, sender string, mo *tpdu.MO) route.Outcome
}

// SMSF is the Nsmsf_SMService producer, the per-UE CP layer and the router's NAS domain.
type SMSF struct {
	Router Router // MO messages; set before Serve

	cfg      Config
	sub      api.SubscriberBackend // a UE's number when the AMF sends no gpsi; may be nil
	store    *store
	amf      *amfClient
	nrf      *nrfClient // nil without an NRF
	retryGap time.Duration
	seq      atomic.Uint64

	mu      sync.Mutex
	workers map[string]*worker // supi -> the goroutine owning that UE's CP state
	txns    map[string]string  // MT transaction id (in the failure callback URI) -> supi

	refusedMu sync.Mutex
	refused   map[netip.Addr]bool // untrusted callers already logged (guard)
}

// New builds the SMSF and loads the saved SMS contexts. sub may be nil.
func New(cfg Config, sub api.SubscriberBackend) (*SMSF, error) {
	if cfg.APIRoot == "" {
		return nil, errors.New("smsf: apiRoot is required (the AMF and the failure callback use it)")
	}
	if cfg.NRF == "" && cfg.AMF == "" {
		return nil, errors.New("smsf: amf is required without an nrf")
	}
	if cfg.NRF != "" && cfg.NFInstanceID == "" {
		return nil, errors.New("smsf: nfInstanceId is required with an nrf")
	}
	if len(cfg.Trusted) == 0 {
		return nil, errors.New("smsf: trusted is required (the AMF's addresses): without it anyone could send sms as any subscriber")
	}
	cfg.APIRoot = strings.TrimRight(cfg.APIRoot, "/")
	if cfg.Listen == "" {
		cfg.Listen = ":29540"
	}
	if cfg.ContextMaxAge <= 0 {
		cfg.ContextMaxAge = 24 * time.Hour
	}
	if cfg.TC1 <= 0 {
		cfg.TC1 = 6 * time.Second
	}
	if cfg.MaxRetrans <= 0 {
		cfg.MaxRetrans = 2
	}
	if cfg.PagingWait <= 0 {
		cfg.PagingWait = 12 * time.Second
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	if cfg.Insecure {
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	s := &SMSF{
		cfg: cfg, sub: sub, store: newStore(cfg.StateFile), retryGap: time.Second,
		workers: map[string]*worker{}, txns: map[string]string{},
		amf: &amfClient{hc: hc, sbi: &sbi.Client{Token: cfg.Token}, nrf: strings.TrimRight(cfg.NRF, "/"),
			static: strings.TrimRight(cfg.AMF, "/"), cache: map[string]string{}},
	}
	if cfg.NRF != "" {
		n, err := newNRFClient(cfg, hc)
		if err != nil {
			return nil, err
		}
		s.nrf = n
	}
	n, err := s.store.load(cfg.ContextMaxAge, time.Now())
	if err != nil {
		return nil, err
	}
	if n > 0 {
		log.Printf("smsf: restored %d sms context(s) from %s", n, cfg.StateFile)
	}
	return s, nil
}

// Close deregisters from the NRF and writes the SMS contexts out.
func (s *SMSF) Close() error {
	if s.nrf != nil {
		s.nrf.close()
	}
	return s.store.save()
}

// Reachable says whether a number gets its MT messages over NAS: one SIM with an SMS context has it.
func (s *SMSF) Reachable(msisdn string) bool {
	_, ok := s.reachable(msisdn)
	return ok
}

func (s *SMSF) reachable(msisdn string) (string, bool) {
	supi, ok := s.store.lookup(msisdn)
	// the AMF finds a UE context by imsi-, imei- or 5g-guti-; an SMS context only ever names a SUPI
	return supi, ok && strings.HasPrefix(supi, "imsi-")
}

// Deliver sends an MT RP-DATA to the recipient over NAS and waits for the phone's RP report, or ctx.
func (s *SMSF) Deliver(ctx context.Context, recipient string, rpData []byte) route.Attempt {
	supi, ok := s.reachable(recipient)
	if !ok {
		return route.Failed(tpdu.RPCauseDestOutOfOrder)
	}
	if len(rpData) > cp.MaxRPDU || len(rpData) < 2 {
		return route.Failed(tpdu.RPCauseNetworkOutOfOrder)
	}
	w := s.workerFor(supi)
	if w == nil {
		return route.Failed(tpdu.RPCauseDestOutOfOrder)
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, route.DefaultDeadline)
		defer cancel()
	}
	req := &mtReq{ctx: ctx, rp: rpData, done: make(chan route.Attempt, 1)}
	if !w.post(ctx, event{kind: evDeliver, mt: req}) {
		return route.Failed(tpdu.RPCauseDestOutOfOrder)
	}
	select {
	case a := <-req.done:
		return a
	case <-ctx.Done():
		// the phone took the CP-DATA but its RP report did not come in time: it most likely has the message, and
		// telling the sender it failed would only bring a duplicate
		if req.acked.Load() {
			return route.Delivered
		}
		return route.Failed(tpdu.RPCauseTemporaryFailure)
	}
}

// workerFor returns the goroutine owning supi's CP state, starting it if the UE has an SMS context.
func (s *SMSF) workerFor(supi string) *worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.workers[supi]; w != nil {
		return w
	}
	if _, ok := s.store.get(supi); !ok {
		return nil
	}
	w := newWorker(s, supi)
	s.workers[supi] = w
	go w.run()
	return w
}

// stopWorker ends supi's worker; its pending MT deliveries fail.
func (s *SMSF) stopWorker(supi string) {
	s.mu.Lock()
	w := s.workers[supi]
	s.mu.Unlock()
	if w != nil {
		w.retire()
	}
}

func (s *SMSF) notifURI(txn string) string {
	return s.cfg.APIRoot + "/nsmsf-callback/v1/n1n2-failure/" + txn
}
