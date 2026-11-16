// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package smsf

import (
	"bytes"
	"context"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coranlabs/SETU/sms/cp"
	"github.com/coranlabs/SETU/sms/route"
	"github.com/coranlabs/SETU/sms/tpdu"
)

// ---- MO: the phone sends ----

// the whole MO transaction: CP-ACK first, then the RP-ACK in a CP-DATA, and the phone's CP-ACK ends it
func TestMOTransaction(t *testing.T) {
	amf := newFakeAMF(t)
	rt := &fakeRouter{out: route.Outcome{Delivered: true, Domain: "ims"}}
	_, srv := newTestSMSF(t, amf.srv.URL, rt, nil)
	activate(t, srv, supiA, numA)
	ue(t, srv, supiA, cp.EncodeData(false, 2, moRP(numB, 9)))
	ack := amf.next(t, supiA)
	if !bytes.Equal(ack.cp, []byte{0xa9, 0x04}) {
		t.Fatalf("first N1N2 % x, want the CP-ACK a9 04", ack.cp)
	}
	rep := amf.next(t, supiA)
	if !bytes.Equal(rep.cp, []byte{0xa9, 0x01, 0x02, 0x03, 0x09}) {
		t.Fatalf("report % x, want CP-DATA a9 01 02 + RP-ACK 03 09", rep.cp)
	}
	if got := rt.seen(); len(got) != 1 || got[0] != numA+">"+numB {
		t.Fatalf("routed %v, want the sender's number to the recipient", got)
	}
	if rep.req.LastMsgIndication || rep.req.N1MessageContainer.N1MessageClass != "SMS" || rep.req.N1N2FailureTxfNotifURI != "" {
		t.Fatalf("report JSON %s", rep.raw)
	}
	ue(t, srv, supiA, cp.EncodeAck(false, 2))
	amf.none(t, supiA, 3*tc1+100*time.Millisecond)
}

// the RP-ERROR carries the router's cause
func TestMOFailedReport(t *testing.T) {
	amf := newFakeAMF(t)
	_, srv := newTestSMSF(t, amf.srv.URL, &fakeRouter{out: route.Outcome{Cause: 27}}, nil)
	activate(t, srv, supiA, numA)
	ue(t, srv, supiA, cp.EncodeData(false, 0, moRP(numB, 4)))
	amf.next(t, supiA)
	if rep := amf.next(t, supiA); !bytes.Equal(rep.cp, []byte{0x89, 0x01, 0x04, 0x05, 0x04, 0x01, 0x1b}) {
		t.Fatalf("report % x, want RP-ERROR 05 04 01 1b", rep.cp)
	}
}

// without the phone's CP-ACK the report goes out again on TC1*, maxRetrans times, then the SMSF gives up
func TestMORetransmitsTheReport(t *testing.T) {
	amf := newFakeAMF(t)
	_, srv := newTestSMSF(t, amf.srv.URL, &fakeRouter{out: route.Outcome{Delivered: true}}, nil)
	activate(t, srv, supiA, numA)
	ue(t, srv, supiA, cp.EncodeData(false, 1, moRP(numB, 7)))
	amf.next(t, supiA) // CP-ACK
	first := amf.next(t, supiA)
	for i := 1; i <= 2; i++ {
		m := amf.next(t, supiA)
		if !bytes.Equal(m.cp, first.cp) {
			t.Fatalf("retransmission %d: % x, want % x", i, m.cp, first.cp)
		}
		if gap := m.at.Sub(first.at); gap < time.Duration(i)*tc1-20*time.Millisecond {
			t.Fatalf("retransmission %d after %v, TC1* is %v", i, gap, tc1)
		}
	}
	amf.none(t, supiA, 2*tc1)
}

// a CP-DATA on a new TI acknowledges the report still waiting on the old one
func TestMOImplicitAck(t *testing.T) {
	amf := newFakeAMF(t)
	_, srv := newTestSMSF(t, amf.srv.URL, &fakeRouter{out: route.Outcome{Delivered: true}}, nil)
	activate(t, srv, supiA, numA)
	ue(t, srv, supiA, cp.EncodeData(false, 0, moRP(numB, 1)))
	amf.next(t, supiA)                            // CP-ACK ti 0
	if r := amf.next(t, supiA); r.cp[0] != 0x89 { // report ti 0
		t.Fatalf("report % x", r.cp)
	}
	ue(t, srv, supiA, cp.EncodeData(false, 1, moRP(numB, 2)))
	if m := amf.next(t, supiA); !bytes.Equal(m.cp, []byte{0x99, 0x04}) {
		t.Fatalf("% x, want the CP-ACK for ti 1", m.cp)
	}
	if r := amf.next(t, supiA); !bytes.Equal(r.cp, []byte{0x99, 0x01, 0x02, 0x03, 0x02}) {
		t.Fatalf("% x, want the report for ti 1", r.cp)
	}
	ue(t, srv, supiA, cp.EncodeAck(false, 1))
	// ti 0's report is never sent again: the second message acknowledged it
	amf.none(t, supiA, 3*tc1)
}

// the phone repeating its CP-DATA (it missed the CP-ACK) gets the CP-ACK again; the SMS is routed once
func TestMODuplicate(t *testing.T) {
	amf := newFakeAMF(t)
	rt := &fakeRouter{out: route.Outcome{Delivered: true}, hold: make(chan struct{})}
	_, srv := newTestSMSF(t, amf.srv.URL, rt, nil)
	activate(t, srv, supiA, numA)
	msg := cp.EncodeData(false, 3, moRP(numB, 5))
	ue(t, srv, supiA, msg)
	ue(t, srv, supiA, msg)
	for range 2 {
		if m := amf.next(t, supiA); !bytes.Equal(m.cp, []byte{0xb9, 0x04}) {
			t.Fatalf("% x, want the CP-ACK b9 04", m.cp)
		}
	}
	close(rt.hold)
	if r := amf.next(t, supiA); !bytes.Equal(r.cp, []byte{0xb9, 0x01, 0x02, 0x03, 0x05}) {
		t.Fatalf("report % x", r.cp)
	}
	ue(t, srv, supiA, cp.EncodeAck(false, 3))
	amf.none(t, supiA, 2*tc1)
	if n := len(rt.seen()); n != 1 {
		t.Fatalf("routed %d times", n)
	}
}

// RP-SMMA (memory available) is answered with an RP-ACK at once; another RP type gets RP-ERROR 97
func TestMOSMMAAndOtherTypes(t *testing.T) {
	amf := newFakeAMF(t)
	rt := &fakeRouter{}
	_, srv := newTestSMSF(t, amf.srv.URL, rt, nil)
	activate(t, srv, supiA, numA)
	ue(t, srv, supiA, cp.EncodeData(false, 0, []byte{tpdu.RPSMMA, 0x05}))
	amf.next(t, supiA)
	if r := amf.next(t, supiA); !bytes.Equal(r.cp, []byte{0x89, 0x01, 0x02, 0x03, 0x05}) {
		t.Fatalf("SMMA answer % x, want RP-ACK 03 05", r.cp)
	}
	ue(t, srv, supiA, cp.EncodeData(false, 1, []byte{tpdu.RPAckMS, 0x06}))
	amf.next(t, supiA)
	if r := amf.next(t, supiA); !bytes.Equal(r.cp, []byte{0x99, 0x01, 0x04, 0x05, 0x06, 0x01, 0x61}) {
		t.Fatalf("answer % x, want RP-ERROR 05 06 01 61", r.cp)
	}
	if n := len(rt.seen()); n != 0 {
		t.Fatalf("routed %d", n)
	}
}

// ---- MT: the phone receives ----

func mtRP(ref byte) []byte {
	return tpdu.EncodeDeliverWith(numA, "Hi", time.Date(2025, 8, 6, 0, 0, 0, 0, time.UTC), "7", ref)
}

func deliver(s *SMSF, to string, rp []byte, d time.Duration) chan route.Attempt {
	ch := make(chan route.Attempt, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		ch <- s.Deliver(ctx, to, rp)
	}()
	return ch
}

func result(t *testing.T, ch chan route.Attempt) route.Attempt {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("delivery never ended")
		return route.Attempt{}
	}
}

// an MT delivered over NAS: the N1N2 shape, the phone's CP-ACK and RP-ACK, the closing CP-ACK
func TestMTDelivered(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	activate(t, srv, supiB, numB)
	rp := mtRP(0x21)
	ch := deliver(s, numB, rp, 3*time.Second)
	m := amf.next(t, supiB)

	mt, params, err := mime.ParseMediaType(m.ctype)
	if err != nil || mt != "multipart/related" || params["type"] != "application/json" || params["boundary"] == "" {
		t.Fatalf("Content-Type %q", m.ctype)
	}
	if len(m.parts) != 2 || m.parts[0].ctype != "application/json" || m.parts[0].id != "jsonData" ||
		m.parts[1].ctype != "application/vnd.3gpp.5gnas" || m.parts[1].id != "binaryDataN1Message" {
		t.Fatalf("parts %+v", m.parts)
	}
	c := m.req.N1MessageContainer
	if c.N1MessageClass != "SMS" || c.N1MessageContent.ContentID != "binaryDataN1Message" || m.ue != supiB ||
		!strings.HasPrefix(m.req.N1N2FailureTxfNotifURI, "http://smsf.test:29540/nsmsf-callback/v1/n1n2-failure/") {
		t.Fatalf("N1N2 to %s: %s", m.ue, m.raw)
	}
	if strings.Contains(m.raw, "pduSessionId") || strings.Contains(m.raw, "n2InfoContainer") {
		t.Fatalf("SMS N1N2 carries SM fields: %s", m.raw)
	}
	if want := append([]byte{0x09, 0x01, byte(len(rp))}, rp...); !bytes.Equal(m.cp, want) {
		t.Fatalf("CP-DATA % x, want % x", m.cp, want)
	}

	ue(t, srv, supiB, cp.EncodeAck(true, 0))
	ue(t, srv, supiB, cp.EncodeData(true, 0, []byte{tpdu.RPAckMS, 0x21, 0x41, 0x02, 0x00, 0x00}))
	if a := result(t, ch); a != route.Delivered {
		t.Fatalf("result %+v", a)
	}
	fin := amf.next(t, supiB)
	if !bytes.Equal(fin.cp, []byte{0x09, 0x04}) || !fin.req.LastMsgIndication {
		t.Fatalf("closing N1N2 % x last=%v, want CP-ACK 09 04 with lastMsgIndication", fin.cp, fin.req.LastMsgIndication)
	}
}

// the phone's RP-ERROR fails the delivery with 21; the CP layer is still closed with a CP-ACK
func TestMTRefused(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	activate(t, srv, supiB, numB)
	ch := deliver(s, numB, mtRP(0x22), 3*time.Second)
	amf.next(t, supiB)
	// no CP-ACK first: the CP-DATA stands in for it
	ue(t, srv, supiB, cp.EncodeData(true, 0, []byte{tpdu.RPErrorMS, 0x22, 0x01, 0x16}))
	if a := result(t, ch); a != route.Failed(21) {
		t.Fatalf("result %+v, want Failed(21)", a)
	}
	if fin := amf.next(t, supiB); !bytes.Equal(fin.cp, []byte{0x09, 0x04}) {
		t.Fatalf("closing % x", fin.cp)
	}
}

// 202: the AMF pages; nothing is resent meanwhile, and its failure callback fails the delivery
func TestMTPagingFails(t *testing.T) {
	amf := newFakeAMF(t)
	amf.set(func(n1msg) int { return http.StatusAccepted })
	s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	activate(t, srv, supiB, numB)
	ch := deliver(s, numB, mtRP(1), 3*time.Second)
	m := amf.next(t, supiB)
	amf.none(t, supiB, 150*time.Millisecond) // the paging wait is 300 ms
	txn := path.Base(m.req.N1N2FailureTxfNotifURI)
	st, _, _ := do(t, http.MethodPost, srv.URL+"/nsmsf-callback/v1/n1n2-failure/"+txn, "application/json",
		`{"cause":"UE_NOT_RESPONDING","n1n2MsgDataUri":"http://amf/x"}`)
	if st != http.StatusNoContent {
		t.Fatalf("callback %d", st)
	}
	if a := result(t, ch); a != route.Failed(27) {
		t.Fatalf("result %+v", a)
	}
	// a callback for a transaction nobody knows is still 204
	if st, _, _ := do(t, http.MethodPost, srv.URL+"/nsmsf-callback/v1/n1n2-failure/nope", "application/json", `{}`); st != 204 {
		t.Fatalf("unknown callback %d", st)
	}
}

// 202 and then the phone answers: paging worked
func TestMTPagingSucceeds(t *testing.T) {
	amf := newFakeAMF(t)
	amf.set(func(n1msg) int { return http.StatusAccepted })
	s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	activate(t, srv, supiB, numB)
	ch := deliver(s, numB, mtRP(1), 3*time.Second)
	amf.next(t, supiB)
	amf.set(func(n1msg) int { return http.StatusOK })
	ue(t, srv, supiB, cp.EncodeAck(true, 0))
	ue(t, srv, supiB, cp.EncodeData(true, 0, []byte{tpdu.RPAckMS, 0x01}))
	if a := result(t, ch); a != route.Delivered {
		t.Fatalf("result %+v", a)
	}
}

// 409 is retried three times, retryGap apart
func TestMTBusyRetries(t *testing.T) {
	amf := newFakeAMF(t)
	var n atomic.Int32
	amf.set(func(n1msg) int {
		if n.Add(1) <= 2 {
			return http.StatusConflict
		}
		return http.StatusOK
	})
	s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	activate(t, srv, supiB, numB)
	ch := deliver(s, numB, mtRP(3), 3*time.Second)
	m1, m2, m3 := amf.next(t, supiB), amf.next(t, supiB), amf.next(t, supiB)
	if !bytes.Equal(m1.cp, m2.cp) || !bytes.Equal(m2.cp, m3.cp) || m2.at.Sub(m1.at) < 25*time.Millisecond {
		t.Fatalf("retries % x / % x / % x, %v apart", m1.cp, m2.cp, m3.cp, m2.at.Sub(m1.at))
	}
	ue(t, srv, supiB, cp.EncodeAck(true, 0))
	ue(t, srv, supiB, cp.EncodeData(true, 0, []byte{tpdu.RPAckMS, 0x03}))
	if a := result(t, ch); a != route.Delivered {
		t.Fatalf("result %+v", a)
	}

	// always busy: one send and three retries, then it fails
	n.Store(-100)
	ch = deliver(s, numB, mtRP(4), 3*time.Second)
	if a := result(t, ch); a != route.Failed(27) {
		t.Fatalf("always busy: %+v", a)
	}
}

func TestMTAMFRefuses(t *testing.T) {
	for _, c := range []struct {
		status int
		want   route.Attempt
		drops  bool
	}{
		{http.StatusNotFound, route.Failed(27), true},
		{http.StatusForbidden, route.Failed(27), false},
		{http.StatusGatewayTimeout, route.Failed(27), false},
		{http.StatusBadRequest, route.Failed(38), false},
		{http.StatusInternalServerError, route.Failed(38), false},
	} {
		amf := newFakeAMF(t)
		amf.set(func(n1msg) int { return c.status })
		s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
		activate(t, srv, supiB, numB)
		if a := result(t, deliver(s, numB, mtRP(1), 3*time.Second)); a != c.want {
			t.Errorf("%d: %+v, want %+v", c.status, a, c.want)
		}
		if _, has := s.store.get(supiB); has == c.drops {
			t.Errorf("%d: context kept=%v", c.status, has)
		}
	}
	// the AMF not answering at all is a network failure
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	activate(t, srv, supiB, numB)
	amf.srv.Close()
	if a := result(t, deliver(s, numB, mtRP(1), 3*time.Second)); a != route.Failed(38) {
		t.Errorf("AMF down: %+v", a)
	}
}

// the phone never acknowledging: CP-DATA, two retransmissions on TC1*, then failure
func TestMTTC1RunsOut(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	activate(t, srv, supiB, numB)
	ch := deliver(s, numB, mtRP(5), 3*time.Second)
	for range 3 {
		amf.next(t, supiB)
	}
	if a := result(t, ch); a != route.Failed(27) {
		t.Fatalf("result %+v", a)
	}
	amf.none(t, supiB, 2*tc1)
}

// CP-ACK in but the RP report late: at the deadline the message counts as delivered (no duplicate); with no CP-ACK
// at all it is a temporary failure
func TestMTDeadline(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, func(c *Config) { c.TC1 = 2 * time.Second })
	activate(t, srv, supiB, numB)
	ch := deliver(s, numB, mtRP(6), 300*time.Millisecond)
	amf.next(t, supiB)
	ue(t, srv, supiB, cp.EncodeAck(true, 0))
	if a := result(t, ch); a != route.Delivered {
		t.Fatalf("acked, no report: %+v", a)
	}
	// the late report is still acknowledged at the CP layer
	ue(t, srv, supiB, cp.EncodeData(true, 0, []byte{tpdu.RPAckMS, 0x06}))
	if m := amf.next(t, supiB); !bytes.Equal(m.cp, []byte{0x09, 0x04}) {
		t.Fatalf("late report answered % x", m.cp)
	}

	ch = deliver(s, numB, mtRP(7), 200*time.Millisecond)
	amf.next(t, supiB)
	if a := result(t, ch); a != route.Failed(41) {
		t.Fatalf("no CP-ACK: %+v", a)
	}
}

// one MT at a time per phone: the second waits until the first is closed, then gets its own TI
func TestMTOneAtATime(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, func(c *Config) { c.TC1 = 2 * time.Second })
	activate(t, srv, supiB, numB)
	ch1 := deliver(s, numB, mtRP(0x31), 3*time.Second)
	m1 := amf.next(t, supiB)
	ch2 := deliver(s, numB, mtRP(0x32), 3*time.Second)
	amf.none(t, supiB, 150*time.Millisecond)
	ue(t, srv, supiB, cp.EncodeAck(true, 0))
	ue(t, srv, supiB, cp.EncodeData(true, 0, []byte{tpdu.RPAckMS, 0x31}))
	if a := result(t, ch1); a != route.Delivered {
		t.Fatalf("first %+v", a)
	}
	if fin := amf.next(t, supiB); !bytes.Equal(fin.cp, []byte{0x09, 0x04}) || fin.req.LastMsgIndication {
		t.Fatalf("closing the first % x last=%v (another message waits)", fin.cp, fin.req.LastMsgIndication)
	}
	m2 := amf.next(t, supiB)
	if m1.cp[4] != 0x31 || m2.cp[0] != 0x19 || m2.cp[4] != 0x32 {
		t.Fatalf("order % x then % x, want ref 31 on ti 0 then ref 32 on ti 1", m1.cp, m2.cp)
	}
	ue(t, srv, supiB, cp.EncodeData(true, 1, []byte{tpdu.RPAckMS, 0x32}))
	if a := result(t, ch2); a != route.Delivered {
		t.Fatalf("second %+v", a)
	}
}

// deactivating fails the MT in progress, so the router can still try IMS
func TestMTDeactivateFailsPending(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, func(c *Config) { c.TC1 = 2 * time.Second })
	activate(t, srv, supiB, numB)
	ch := deliver(s, numB, mtRP(1), 5*time.Second)
	amf.next(t, supiB)
	do(t, http.MethodDelete, srv.URL+"/nsmsf-sms/v2/ue-contexts/"+supiB, "", "")
	select {
	case a := <-ch:
		if a != route.Failed(27) {
			t.Fatalf("result %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("the pending delivery outlived the context")
	}
}

// a phone answering a transaction the SMSF never opened gets CP-ERROR 81 (invalid TI)
func TestMTUnknownTI(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, nil)
	activate(t, srv, supiB, numB)
	ue(t, srv, supiB, cp.EncodeData(true, 5, []byte{tpdu.RPAckMS, 0x01}))
	if m := amf.next(t, supiB); !bytes.Equal(m.cp, []byte{0x59, 0x10, 0x51}) {
		t.Fatalf("% x, want CP-ERROR 59 10 51", m.cp)
	}
	ue(t, srv, supiB, cp.EncodeError(true, 5, 111)) // an error is never answered
	amf.none(t, supiB, 100*time.Millisecond)
	if a := s.Deliver(context.Background(), "0100009999", mtRP(1)); a != route.Failed(27) {
		t.Fatalf("no context: %+v", a)
	}
}

// one phone sends to another through the real router: MO over NAS from A, MT over NAS to B, the report back to A
func TestMOToAnotherNASPhone(t *testing.T) {
	amf := newFakeAMF(t)
	s, srv := newTestSMSF(t, amf.srv.URL, nil, func(c *Config) { c.TC1 = 2 * time.Second })
	s.Router = &route.Router{NAS: s, Dir: s, SMSC: "7"}
	activate(t, srv, supiA, numA)
	activate(t, srv, supiB, numB)
	ue(t, srv, supiA, cp.EncodeData(false, 1, moRP(numB, 9)))
	if m := amf.next(t, supiA); !bytes.Equal(m.cp, []byte{0x99, 0x04}) {
		t.Fatalf("A got % x first", m.cp)
	}
	mt := amf.next(t, supiB)
	if mt.cp[0] != 0x09 || mt.cp[3] != tpdu.RPDataN || !bytes.Contains(mt.cp, tpdu.EncodeBCDAddr(numA)) {
		t.Fatalf("B got % x", mt.cp)
	}
	mr := mt.cp[4]
	ue(t, srv, supiB, cp.EncodeAck(true, 0))
	ue(t, srv, supiB, cp.EncodeData(true, 0, []byte{tpdu.RPAckMS, mr}))
	if m := amf.next(t, supiB); !bytes.Equal(m.cp, []byte{0x09, 0x04}) {
		t.Fatalf("B's closing CP-ACK % x", m.cp)
	}
	if rep := amf.next(t, supiA); !bytes.Equal(rep.cp, []byte{0x99, 0x01, 0x02, 0x03, 0x09}) {
		t.Fatalf("A's report % x, want RP-ACK 03 09", rep.cp)
	}
}
