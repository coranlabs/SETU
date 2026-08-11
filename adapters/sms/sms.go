// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package sms relays SMS over IP between UEs (3GPP TS 24.011/23.040). The S-CSCF
// posts a mobile-originated RP-DATA to /mo; the adapter decodes it, injects the
// mobile-terminated MESSAGE towards the recipient and returns a submit report.
package sms

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/coranlabs/SETU/uilog"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coranlabs/SETU/sms/tpdu"
)

type Adapter struct {
	Listen  string // e.g. 127.0.0.1:8090
	SCSCF   string // e.g. 192.0.2.10:6060 (UDP SIP)
	SelfIP  string // advertised in Via
	ViaPort int    // advertised Via port (proven system used 8091)
	Domain  string // e.g. ims.mnc001.mcc001.3gppnetwork.org
	Now     func() time.Time
}

type moReq struct {
	From string `json:"from"`
	TPDU string `json:"tpdu"`
}
type moResp struct {
	Recipient  string `json:"recipient"`
	Text       string `json:"text"`
	DeliverHex string `json:"deliverHex"`
	Err        string `json:"err,omitempty"`
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// HandleMO is the POST /mo handler (exported for tests).
func (a *Adapter) HandleMO(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("sms: recovered panic: %v", rec)
		}
	}()
	body, _ := io.ReadAll(r.Body)
	var in moReq
	_ = json.Unmarshal(body, &in)
	rp, err := hex.DecodeString(strings.TrimSpace(in.TPDU))
	out := moResp{}
	if err != nil {
		out.Err = "bad hex: " + err.Error()
	} else if mo, e := tpdu.DecodeMO(rp); e != nil {
		out.Err = "decode: " + e.Error()
	} else {
		out.Recipient = mo.Recipient
		out.Text = mo.Text
		fromNum := strings.TrimPrefix(in.From, "+")
		deliver := tpdu.EncodeDeliver(fromNum, mo.Text, a.now())
		out.DeliverHex = hex.EncodeToString(deliver)
		if e := a.sendMT(fromNum, mo.Recipient, deliver); e != nil {
			uilog.Event(uilog.Red, "⛔", "SMS-FAIL", "%s → %s: %v", in.From, out.Recipient, e)
			out.Err = "mt: " + e.Error()
		} else {
			uilog.Event(uilog.Yellow, "💬", "SMS", "%s → %s  %q  ✓ delivered", in.From, out.Recipient, out.Text)
		}
		// Submit report back to the sender so the handset shows "sent". Sent
		// regardless of the MT result; RP-ERROR handling is not implemented.
		if e := a.sendMT(mo.Recipient, fromNum, tpdu.BuildRPAck(mo.RPRef)); e != nil {
			uilog.Event(uilog.Red, "⛔", "ACK-FAIL", "to %s: %v", fromNum, e)
		} else {
			uilog.Event(uilog.Yellow, "✅", "SMS-ACK", "to %s  submit-report sent  · ref %d", fromNum, mo.RPRef)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// sendMT sends a SIP MESSAGE to the S-CSCF over UDP. X-SMS-Relay marks it as
// already relayed so the S-CSCF routes it terminating instead of intercepting it
// again.
func (a *Adapter) sendMT(sender, recipient string, payload []byte) error {
	ruri := fmt.Sprintf("sip:%s@%s", recipient, a.Domain)
	rnd := func() int { return rand.IntN(1 << 30) }
	hdr := fmt.Sprintf("MESSAGE %s SIP/2.0\r\nVia: SIP/2.0/UDP %s:%d;branch=z9hG4bKsms%d\r\nMax-Forwards: 70\r\nX-SMS-Relay: mt\r\nFrom: <sip:%s@%s>;tag=%d\r\nTo: <sip:%s@%s>\r\nCall-ID: sms%d%d@%s\r\nCSeq: 1 MESSAGE\r\nP-Asserted-Identity: <sip:%s@%s>\r\nContent-Type: application/vnd.3gpp.sms\r\nContent-Length: %d\r\n\r\n",
		ruri, a.SelfIP, a.ViaPort, rnd(), sender, a.Domain, rnd(), recipient, a.Domain, rnd(), rnd(), a.SelfIP, sender, a.Domain, len(payload))
	pkt := append([]byte(hdr), payload...)
	conn, err := net.Dial("udp", a.SCSCF)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(pkt)
	return err
}

// Serve runs the MO ingest HTTP server until it fails.
func (a *Adapter) Serve() error {
	if a.ViaPort == 0 {
		a.ViaPort = 8091
	}
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
