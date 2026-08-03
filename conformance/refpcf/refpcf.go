// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package refpcf is a conformant *reference* implementation of the SD-Core PCF's
// Npcf_PolicyAuthorization (N5) server contract, for driving the rx-n5-gateway at
// scale when a real SD-Core PCF is not available (the [HERE] test tier).
//
// It is NOT a stub: it validates every AppSessionContext the gateway POSTs (and
// rejects malformed ones with 400), hands out real Location URLs, honours
// PATCH/DELETE, and — like the real PCF — autonomously calls BACK to the AF
// (the gateway's per-session notifUri) to deliver QoS-change notifications
// (-> Diameter RAR) and terminations (-> Diameter ASR). What it does NOT reproduce
// is the real PCF's policy engine, its h2c transport, or the notifUri "/terminate"
// suffix — those divergences are catalogued in INTEGRATION-REALITY-REVIEW.md and are
// out of scope for a load/observability harness. Stdlib only.
package refpcf

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coranlabs/SETU/chassis/metrics"
	"github.com/coranlabs/SETU/connectors/sdcore/rx2n5"
)

// PCF is a reference N5 server. Mount Handler() and, optionally, run Notifier() to
// generate network-initiated events against the sessions it is holding.
type PCF struct {
	basePath string // e.g. "/npcf-policyauthorization/v1"
	http     *http.Client

	mu    sync.Mutex
	next  int64
	sess  map[string]*appSession // appSessionId -> session
	byLoc map[string]string      // Location path -> appSessionId

	Metrics                        *metrics.Registry
	mCreate, mPatch, mDelete       *metrics.Counter
	mReject, mQoSNotif, mTermNotif *metrics.Counter
	mNotifErr                      *metrics.Counter
	mActive                        *metrics.Gauge

	rnd *rand.Rand
}

type appSession struct {
	id       string
	notifURI string
	created  time.Time
	medComps int
}

// New builds a reference PCF served under basePath (which must match the gateway's
// -pcf URL path, e.g. "/npcf-policyauthorization/v1"). httpc is used for the AF
// callbacks; pass a client with a short timeout. seed makes the notifier
// deterministic in tests.
func New(basePath string, httpc *http.Client, seed int64) *PCF {
	if httpc == nil {
		httpc = &http.Client{Timeout: 3 * time.Second}
	}
	reg := metrics.New()
	p := &PCF{
		basePath: strings.TrimRight(basePath, "/"),
		http:     httpc,
		sess:     map[string]*appSession{},
		byLoc:    map[string]string{},
		Metrics:  reg,
		rnd:      rand.New(rand.NewSource(seed)),
	}
	p.mCreate = reg.Counter("refpcf_appsession_create_total", "N5 app-sessions created")
	p.mPatch = reg.Counter("refpcf_appsession_patch_total", "N5 app-sessions patched (re-INVITE)")
	p.mDelete = reg.Counter("refpcf_appsession_delete_total", "N5 app-sessions deleted")
	p.mReject = reg.Counter("refpcf_reject_total", "N5 requests rejected as malformed")
	p.mQoSNotif = reg.Counter("refpcf_qos_notif_total", "QoS-change notifications pushed to the AF")
	p.mTermNotif = reg.Counter("refpcf_termination_notif_total", "termination notifications pushed to the AF")
	p.mNotifErr = reg.Counter("refpcf_notif_errors_total", "AF callbacks that failed")
	p.mActive = reg.Gauge("refpcf_active_sessions", "app-sessions currently held")
	return p
}

// Handler serves the N5 routes.
func (p *PCF) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(p.basePath+"/app-sessions", p.handleCollection)
	mux.HandleFunc(p.basePath+"/app-sessions/", p.handleIndividual)
	return mux
}

func (p *PCF) handleCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var asc rx2n5.AppSessionContext
	if err := json.Unmarshal(body, &asc); err != nil {
		p.mReject.Inc()
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := validate(asc); err != nil {
		p.mReject.Inc()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.next++
	id := fmt.Sprintf("appsess-%d", p.next)
	locPath := p.basePath + "/app-sessions/" + id
	p.sess[id] = &appSession{id: id, notifURI: asc.AscReqData.NotifURI, created: time.Now(), medComps: len(asc.AscReqData.MedComponents)}
	p.byLoc[locPath] = id
	p.mActive.Set(int64(len(p.sess)))
	p.mu.Unlock()
	p.mCreate.Inc()
	w.Header().Set("Location", "http://"+r.Host+locPath)
	w.WriteHeader(http.StatusCreated)
}

func (p *PCF) handleIndividual(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPatch:
		if r.Header.Get("Content-Type") != "application/merge-patch+json" {
			http.Error(w, "bad content-type", http.StatusUnsupportedMediaType)
			return
		}
		io.Copy(io.Discard, r.Body)
		p.mPatch.Inc()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delete"):
		id := p.byLoc[strings.TrimSuffix(r.URL.Path, "/delete")]
		p.mu.Lock()
		if id != "" {
			delete(p.sess, id)
			delete(p.byLoc, strings.TrimSuffix(r.URL.Path, "/delete"))
		}
		p.mActive.Set(int64(len(p.sess)))
		p.mu.Unlock()
		p.mDelete.Inc()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// validate enforces the N5 create contract the gateway must satisfy, so the harness
// actually proves the gateway emits well-formed AppSessionContexts.
func validate(asc rx2n5.AppSessionContext) error {
	a := asc.AscReqData
	if a.DNN == "" || a.Gpsi == "" || a.AfAppID == "" {
		return fmt.Errorf("missing required top-level field")
	}
	if a.UeIPv4 == "" && a.UeIPv6 == "" {
		return fmt.Errorf("missing UE IP")
	}
	if a.NotifURI == "" {
		return fmt.Errorf("missing notifUri")
	}
	if len(a.MedComponents) == 0 {
		return fmt.Errorf("no media components")
	}
	for _, mc := range a.MedComponents {
		if mc.MedType == "" || len(mc.MedSubComps) == 0 {
			return fmt.Errorf("bad media component")
		}
		for _, sc := range mc.MedSubComps {
			if len(sc.FDescs) == 0 || sc.FStatus == "" {
				return fmt.Errorf("bad media sub-component")
			}
		}
	}
	return nil
}

// NotifierConfig controls the autonomous callback load.
type NotifierConfig struct {
	Interval      time.Duration // how often to sweep held sessions
	QoSFraction   float64       // per-sweep probability a session gets a QoS notif (-> RAR)
	TermFraction  float64       // per-sweep probability a session gets a termination (-> ASR)
	MinAgeForTerm time.Duration // don't terminate a call younger than this
}

// Notifier runs until ctx is done (signalled by closing stop), periodically pushing
// QoS-change and termination notifications to the AF notifUris of held sessions —
// exactly the network-initiated events the gateway must translate to RAR/ASR.
func (p *PCF) Notifier(cfg NotifierConfig, stop <-chan struct{}) {
	if cfg.Interval <= 0 {
		cfg.Interval = 500 * time.Millisecond
	}
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p.sweep(cfg)
		}
	}
}

func (p *PCF) sweep(cfg NotifierConfig) {
	type target struct {
		uri  string
		term bool
	}
	var targets []target
	now := time.Now()
	p.mu.Lock()
	for _, s := range p.sess {
		if s.notifURI == "" {
			continue
		}
		if cfg.TermFraction > 0 && now.Sub(s.created) > cfg.MinAgeForTerm && p.rnd.Float64() < cfg.TermFraction {
			targets = append(targets, target{s.notifURI, true})
			continue
		}
		if cfg.QoSFraction > 0 && p.rnd.Float64() < cfg.QoSFraction {
			targets = append(targets, target{s.notifURI, false})
		}
	}
	p.mu.Unlock()
	for _, tg := range targets {
		if tg.term {
			p.post(tg.uri, `{"terminate":true,"cause":"PDU_SESSION_TERMINATION"}`)
			p.mTermNotif.Inc()
		} else {
			p.post(tg.uri, `{"evNotifs":[{"event":"QOS_NOTIF"}]}`)
			p.mQoSNotif.Inc()
		}
	}
}

func (p *PCF) post(uri, body string) {
	resp, err := p.http.Post(uri, "application/json", strings.NewReader(body))
	if err != nil {
		p.mNotifErr.Inc()
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		p.mNotifErr.Inc()
	}
}

// ActiveSessions reports how many app-sessions the PCF is currently holding.
func (p *PCF) ActiveSessions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sess)
}
