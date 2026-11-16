// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package sms is SETU's SMS-over-IP access adapter — the e2e-proven sms-router
// re-homed onto the chassis. Wire contract preserved (migration law #1):
// POST /mo {"from","tpdu"} -> decode SMS-SUBMIT -> route it (IMS or NAS, see sms/route)
// -> MT SIP MESSAGE with X-SMS-Relay: mt to the S-CSCF -> RP-ACK back to the sender.
// The sender now gets an RP-ERROR when the message did not get through, and /mo
// answers before routing starts. TP-SCTS stamps the real time (injectable clock).
package sms

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coranlabs/SETU/sms/route"
	"github.com/coranlabs/SETU/sms/tpdu"
	"github.com/coranlabs/SETU/uilog"
)

// Router delivers an MO message and says what to tell the sender (route.Router).
type Router interface {
	Route(ctx context.Context, sender string, mo *tpdu.MO) route.Outcome
}

type Adapter struct {
	Listen  string // e.g. 127.0.0.1:8090
	SCSCF   string // e.g. 192.0.2.10:6060 (UDP SIP)
	SelfIP  string // advertised in Via
	ViaPort int    // advertised Via port (proven system used 8091)
	Domain  string // e.g. ims.mnc001.mcc001.3gppnetwork.org
	SMSC    string // the SC address: the From of the RP-ACK answering an RP-SMMA
	Now     func() time.Time
	Router  Router   // nil: IMS-only delivery over Link
	Link    *IMSLink // nil: a blind link built from the fields above

	once sync.Once
}

type moReq struct {
	From string `json:"from"`
	TPDU string `json:"tpdu"`
}

// moResp is /mo's answer. The S-CSCF does not read it (it answers the phone 202 at once), so it carries no message
// text, no encoded SMS and no number — personal data that would only leave setu for nothing — just a failure, if any.
type moResp struct {
	Err string `json:"err,omitempty"`
}

// deps fills in the link and the router the adapter was not given.
func (a *Adapter) deps() {
	a.once.Do(func() {
		if a.Link == nil {
			a.Link = &IMSLink{SCSCF: a.SCSCF, SelfIP: a.SelfIP, ViaPort: a.ViaPort, Domain: a.Domain}
		}
		if a.Router == nil {
			a.Router = &route.Router{IMS: a.Link, SMSC: a.SMSC, Now: a.Now}
		}
	})
}

// HandleMO is the POST /mo handler (exported for tests). It answers at once: the S-CSCF's http_client_query holds a
// Kamailio worker until it does, and it ignores the answer anyway. Routing and the report to the sender follow.
func (a *Adapter) HandleMO(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("sms: recovered panic: %v", rec)
		}
	}()
	a.deps()
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	var in moReq
	_ = json.Unmarshal(body, &in)
	fromNum := strings.TrimPrefix(in.From, "+")
	rp, err := hex.DecodeString(strings.TrimSpace(in.TPDU))
	out := moResp{}
	if err != nil {
		out.Err = "bad hex: " + err.Error()
	} else if typ, e := tpdu.RPType(rp); e != nil {
		out.Err = "decode: " + e.Error()
	} else if typ == tpdu.RPSMMA && len(rp) >= 2 {
		// the phone has room for messages again; nothing waits for it, so the answer is a plain RP-ACK
		go a.report(a.SMSC, fromNum, tpdu.BuildRPAck(rp[1]))
	} else if mo, e := tpdu.DecodeMO(rp); e != nil {
		out.Err = "decode: " + e.Error()
	} else {
		go a.route(fromNum, mo)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// route delivers mo and sends the sender its RP-ACK or RP-ERROR, from the recipient's address as before.
func (a *Adapter) route(fromNum string, mo *tpdu.MO) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("sms: recovered panic: %v", rec)
		}
	}()
	out := a.Router.Route(context.Background(), fromNum, mo)
	a.report(mo.Recipient, fromNum, out.Report(mo.RPRef))
}

// report sends an RP-ACK or RP-ERROR to the phone at to.
func (a *Adapter) report(as, to string, rp []byte) {
	if to == "" {
		uilog.Event(uilog.Red, "ACK-FAIL", "the sender has no number: no report sent")
		return
	}
	if e := a.Link.Send(as, to, rp); e != nil {
		uilog.Event(uilog.Red, "ACK-FAIL", "to %s: %v", uilog.Mask(to), e)
		return
	}
	if typ, _ := tpdu.RPType(rp); typ == tpdu.RPErrorN && len(rp) >= 4 {
		uilog.Event(uilog.Orange, "SMS-ERR", "to %s  rp-error sent, cause %d, ref %d", uilog.Mask(to), rp[3], rp[1])
		return
	}
	uilog.Event(uilog.Yellow, "SMS-ACK", "to %s  submit report sent, ref %d", uilog.Mask(to), rp[1])
}

// Serve runs the MO ingest HTTP server until it fails.
func (a *Adapter) Serve() error {
	if a.ViaPort == 0 {
		a.ViaPort = 8091
	}
	a.deps()
	mux := http.NewServeMux()
	mux.HandleFunc("/mo", a.HandleMO)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	log.Printf("sms: MO ingest on %s -> S-CSCF %s", a.Listen, a.SCSCF)
	srv := &http.Server{Addr: a.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return srv.ListenAndServe()
}
