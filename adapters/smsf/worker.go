// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package smsf

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coranlabs/SETU/sms/cp"
	"github.com/coranlabs/SETU/sms/route"
	"github.com/coranlabs/SETU/sms/tpdu"
	"github.com/coranlabs/SETU/uilog"
)

const (
	inbox    = 16 // CP messages and deliveries waiting for one UE's worker
	maxQueue = 8  // MT messages waiting behind the one in progress
	maxBusy  = 3  // 409 retries of an MT CP-DATA
)

type evKind uint8

const (
	evUplink  evKind = iota // a CP message from the phone (sendsms)
	evDeliver               // an MT RP-DATA for the phone
	evRouted                // routing of an MO message finished
	evMOTimer               // TC1* of an MO report
	evMTTimer               // TC1*, the paging wait or a 409 retry of the MT CP-DATA
	evFailure               // the AMF could not page the phone
)

type event struct {
	kind evKind
	msg  cp.Msg
	gate <-chan struct{} // evUplink: closed once the sendsms answer is out
	mt   *mtReq
	ti   uint8
	seq  uint64
	out  route.Outcome
	ref  byte
	txn  string
}

// mtReq is one MT delivery waiting for its result.
type mtReq struct {
	ctx   context.Context
	rp    []byte
	done  chan route.Attempt // buffered: the worker never waits for the reader
	acked atomic.Bool        // the phone took the CP-DATA
}

type moState uint8

const (
	moWaitRP  moState = iota // CP-ACK sent, routing
	moWaitAck                // RP report sent, waiting for the phone's CP-ACK
)

// moTxn is one MO transaction: the phone chose its TI.
type moTxn struct {
	ti     uint8
	seq    uint64
	state  moState
	rpdu   []byte // the phone's RP message: a repeat of it is not a new SMS
	report []byte // the CP-DATA carrying the RP report, kept for retransmission
	sent   int    // retransmissions so far
	tick   uint64
	timer  *time.Timer
}

type mtState uint8

const (
	mtWaitAck  mtState = iota // CP-DATA sent, TC1* running
	mtPaging                  // the AMF answered 202 and pages the phone
	mtBusy                    // the AMF answered 409; retrying
	mtWaitData                // CP-ACK in, waiting for the phone's RP report
)

// mtTxn is the MT transaction in progress: the SMSF chose its TI.
type mtTxn struct {
	req   *mtReq
	ti    uint8
	id    string // names it in the failure callback URI
	msg   []byte // the CP-DATA
	state mtState
	sent  int // TC1* retransmissions
	busy  int // 409 retries
	tick  uint64
	timer *time.Timer
}

// worker owns one UE's CP state. Everything that touches it runs on the worker's goroutine; timers and routing
// post events back instead of sleeping or locking. N1N2 requests are sent from here one by one, so the phone gets
// them in order (the CP-ACK before the RP report).
type worker struct {
	s    *SMSF
	supi string
	in   chan event
	ctx  context.Context
	stop context.CancelFunc

	mo     map[uint8]*moTxn
	mt     *mtTxn
	queue  []*mtReq
	nextTI uint8
	lastTI int // TI of the last finished MT transaction, -1 for none: its late messages are answered, not refused
}

func newWorker(s *SMSF, supi string) *worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &worker{s: s, supi: supi, in: make(chan event, inbox), ctx: ctx, stop: cancel, mo: map[uint8]*moTxn{}, lastTI: -1}
}

// retire stops the worker and takes it out of the SMSF at once, so a context activated again right away gets a new
// worker instead of this one, which is still failing what it had.
func (w *worker) retire() {
	w.s.mu.Lock()
	if w.s.workers[w.supi] == w {
		delete(w.s.workers, w.supi)
	}
	w.s.mu.Unlock()
	w.stop()
}

// post hands ev to the worker, waiting while its inbox is full; false when the worker or ctx ended first.
func (w *worker) post(ctx context.Context, ev event) bool {
	select {
	case w.in <- ev:
		return true
	case <-w.ctx.Done():
		return false
	case <-ctx.Done():
		return false
	}
}

// tryPost hands ev to the worker only if there is room now.
func (w *worker) tryPost(ev event) bool {
	if w.ctx.Err() != nil {
		return false
	}
	select {
	case w.in <- ev:
		return true
	default:
		return false
	}
}

// after posts ev once d has passed.
func (w *worker) after(d time.Duration, ev event) *time.Timer {
	return time.AfterFunc(d, func() { w.post(context.Background(), ev) })
}

func (w *worker) run() {
	defer w.shutdown()
	for {
		var deadline <-chan struct{}
		if w.mt != nil {
			deadline = w.mt.req.ctx.Done()
		}
		select {
		case <-w.ctx.Done():
			return
		case <-deadline:
			w.mtDeadline()
		case ev := <-w.in:
			w.handle(ev)
		}
		// whatever just happened may have closed the MT in progress: the next one waiting goes now
		w.startMT()
	}
}

func (w *worker) handle(ev event) {
	switch ev.kind {
	case evUplink:
		select {
		case <-ev.gate:
		case <-w.ctx.Done():
			return
		}
		if ev.msg.TIFlag {
			w.mtUplink(ev.msg)
		} else {
			w.moUplink(ev.msg)
		}
	case evDeliver:
		w.queueMT(ev.mt)
	case evRouted:
		if x := w.mo[ev.ti]; x != nil && x.seq == ev.seq && x.state == moWaitRP {
			w.moReport(x, ev.out, ev.ref)
		}
	case evMOTimer:
		w.moTimer(ev.ti, ev.seq)
	case evMTTimer:
		w.mtTimer(ev.seq)
	case evFailure:
		if w.mt != nil && w.mt.id == ev.txn {
			uilog.Event(uilog.Orange, "SMS-NAS", "to %s: the amf could not page the phone", uilog.Mask(w.supi))
			w.finishMT(route.Failed(tpdu.RPCauseDestOutOfOrder))
		}
	}
}

// shutdown fails whatever still waits on this UE, so the router can try IMS.
func (w *worker) shutdown() {
	for _, x := range w.mo {
		stopTimer(x.timer)
	}
	gone := route.Failed(tpdu.RPCauseDestOutOfOrder)
	if x := w.mt; x != nil {
		stopTimer(x.timer)
		w.forget(x.id)
		x.req.done <- gone
		w.mt = nil
	}
	for _, r := range w.queue {
		r.done <- gone
	}
	w.queue = nil
	for {
		select {
		case ev := <-w.in:
			if ev.mt != nil {
				ev.mt.done <- gone
			}
		default:
			w.s.mu.Lock()
			if w.s.workers[w.supi] == w {
				delete(w.s.workers, w.supi)
			}
			w.s.mu.Unlock()
			return
		}
	}
}

func stopTimer(t *time.Timer) {
	if t != nil {
		t.Stop()
	}
}

// n1 sends one CP message to the phone through the AMF and returns the HTTP status (0: no answer). A 404 means the
// AMF has no context for the UE any more: the SMS context is stale, and is dropped.
func (w *worker) n1(msg []byte, last bool, notif string) int {
	c, ok := w.s.store.get(w.supi)
	if !ok {
		return 0
	}
	st, err := w.s.amf.transfer(w.ctx, c.AMFID, w.supi, msg, last, notif, w.s.cfg.NFInstanceID)
	if err != nil {
		if w.ctx.Err() == nil {
			uilog.Event(uilog.Red, "SMS-NAS", "to %s: n1n2 transfer: %v", uilog.Mask(w.supi), err)
		}
		return 0
	}
	if st == http.StatusNotFound && w.ctx.Err() == nil {
		uilog.Event(uilog.Orange, "SMSF", "%s: the amf has no context for it: sms over nas off", uilog.Mask(w.supi))
		w.s.store.del(w.supi)
		w.s.saveState()
		w.retire()
	}
	return st
}

// ---- MO: the phone's transactions (TI flag 0 from the phone, 1 from the SMSF) ----

func (w *worker) moUplink(m cp.Msg) {
	t := m.TI
	switch m.Type {
	case cp.TypeData:
		// a CP-DATA on another TI stands in for the CP-ACK a report still waits for (TS 24.011 5.4): a phone sending
		// several SMS skips the last CP-ACK and opens the next transaction
		for ti, x := range w.mo {
			if ti != t && x.state == moWaitAck {
				w.closeMO(ti)
			}
		}
		if x := w.mo[t]; x != nil {
			if bytes.Equal(x.rpdu, m.RPDU) {
				// the same message again: the phone missed the CP-ACK. Acknowledge it; it is already being routed
				w.n1(cp.EncodeAck(true, t), false, "")
				return
			}
			w.closeMO(t) // a new message on a TI the phone has reused
		}
		w.newMO(t, m.RPDU)
	case cp.TypeAck:
		if x := w.mo[t]; x != nil && x.state == moWaitAck {
			w.closeMO(t)
		}
	case cp.TypeError:
		if w.mo[t] != nil {
			uilog.Event(uilog.Orange, "SMS-NAS", "%s: the phone ended its sms with cp cause %d", uilog.Mask(w.supi), m.Cause)
			w.closeMO(t)
		}
	}
}

func (w *worker) newMO(t uint8, rpdu []byte) {
	if len(rpdu) < 2 {
		// no RP message reference to answer with: refuse at the CP layer
		w.n1(cp.EncodeError(true, t, cp.CauseInvalidMandatoryIE), false, "")
		return
	}
	x := &moTxn{ti: t, seq: w.s.seq.Add(1), state: moWaitRP, rpdu: rpdu}
	w.mo[t] = x
	w.n1(cp.EncodeAck(true, t), false, "")
	if w.ctx.Err() != nil {
		return // the AMF has no context for the UE: no report could reach it
	}
	ref := rpdu[1]
	switch rpdu[0] & 0x07 {
	case tpdu.RPDataMS:
		mo, err := tpdu.DecodeMO(rpdu)
		if err != nil {
			w.moReport(x, route.Outcome{Cause: tpdu.RPCauseInvalidMandatory}, ref)
			return
		}
		c, _ := w.s.store.get(w.supi)
		uilog.Event(uilog.Yellow, "SMS-NAS", "%s sent an sms over nas (ti %d)", uilog.Mask(w.supi), t)
		rt, seq := w.s.Router, x.seq
		go func() {
			out := route.Outcome{Cause: tpdu.RPCauseNetworkOutOfOrder}
			if rt != nil {
				out = rt.Route(context.Background(), c.MSISDN, mo)
			}
			w.post(context.Background(), event{kind: evRouted, ti: t, seq: seq, out: out, ref: ref})
		}()
	case tpdu.RPSMMA:
		w.moReport(x, route.Outcome{Delivered: true}, ref)
	default:
		w.moReport(x, route.Outcome{Cause: tpdu.RPCauseUnknownType}, ref)
	}
}

// moReport sends the RP-ACK or RP-ERROR in a CP-DATA and waits for the phone's CP-ACK with TC1*.
func (w *worker) moReport(x *moTxn, out route.Outcome, ref byte) {
	x.report = cp.EncodeData(true, x.ti, out.Report(ref))
	x.state = moWaitAck
	w.n1(x.report, false, "")
	w.armMO(x)
}

func (w *worker) armMO(x *moTxn) {
	stopTimer(x.timer)
	x.tick = w.s.seq.Add(1)
	x.timer = w.after(w.s.cfg.TC1, event{kind: evMOTimer, ti: x.ti, seq: x.tick})
}

func (w *worker) moTimer(t uint8, tick uint64) {
	x := w.mo[t]
	if x == nil || x.tick != tick || x.state != moWaitAck {
		return
	}
	if x.sent >= w.s.cfg.MaxRetrans {
		uilog.Event(uilog.Orange, "SMS-NAS", "%s did not acknowledge its sms report (ti %d)", uilog.Mask(w.supi), t)
		w.closeMO(t)
		return
	}
	x.sent++
	w.n1(x.report, false, "")
	w.armMO(x)
}

func (w *worker) closeMO(t uint8) {
	if x := w.mo[t]; x != nil {
		stopTimer(x.timer)
		delete(w.mo, t)
	}
}

// ---- MT: the SMSF's transactions (TI flag 0 from the SMSF, 1 from the phone) ----

func (w *worker) queueMT(r *mtReq) {
	if w.mt != nil && len(w.queue) >= maxQueue {
		r.done <- route.Failed(tpdu.RPCauseTemporaryFailure)
		return
	}
	w.queue = append(w.queue, r)
}

// startMT opens the next MT transaction when none is in progress: a phone takes one MT message at a time.
func (w *worker) startMT() {
	for w.mt == nil && len(w.queue) > 0 && w.ctx.Err() == nil {
		r := w.queue[0]
		w.queue = w.queue[1:]
		if r.ctx.Err() != nil {
			r.done <- route.Failed(tpdu.RPCauseTemporaryFailure)
			continue
		}
		ti := w.allocTI()
		x := &mtTxn{req: r, ti: ti, id: newTxnID(), msg: cp.EncodeData(false, ti, r.rp)}
		w.mt = x
		w.s.mu.Lock()
		w.s.txns[x.id] = w.supi
		w.s.mu.Unlock()
		w.sendMT()
	}
}

// allocTI picks the next TI value 0-6, never the one just finished, so its late messages can be told apart.
func (w *worker) allocTI() uint8 {
	for {
		ti := w.nextTI
		w.nextTI = (w.nextTI + 1) % 7
		if int(ti) != w.lastTI {
			return ti
		}
	}
}

func newTxnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (w *worker) forget(id string) {
	w.s.mu.Lock()
	delete(w.s.txns, id)
	w.s.mu.Unlock()
}

// sendMT sends (or resends) the MT CP-DATA and acts on the AMF's answer.
func (w *worker) sendMT() {
	x := w.mt
	st := w.n1(x.msg, false, w.s.notifURI(x.id))
	if w.mt != x {
		return
	}
	switch {
	case st == http.StatusAccepted:
		// the AMF pages the phone and delivers after its Service Request; sending again now would only meet a 409
		x.state = mtPaging
		w.armMT(x, w.s.cfg.PagingWait)
	case st >= 200 && st < 300:
		x.state = mtWaitAck
		w.armMT(x, w.s.cfg.TC1)
	case st == http.StatusConflict && x.busy < maxBusy:
		// a registration, handover or paging is in progress at the AMF
		x.busy++
		x.state = mtBusy
		w.armMT(x, w.s.retryGap)
	case st == http.StatusConflict, st == http.StatusForbidden, st == http.StatusNotFound, st == http.StatusGatewayTimeout:
		w.finishMT(route.Failed(tpdu.RPCauseDestOutOfOrder))
	default:
		w.finishMT(route.Failed(tpdu.RPCauseNetworkOutOfOrder))
	}
}

func (w *worker) armMT(x *mtTxn, d time.Duration) {
	stopTimer(x.timer)
	x.tick = w.s.seq.Add(1)
	x.timer = w.after(d, event{kind: evMTTimer, seq: x.tick})
}

func (w *worker) mtTimer(tick uint64) {
	x := w.mt
	if x == nil || x.tick != tick {
		return
	}
	switch x.state {
	case mtBusy:
		w.sendMT()
	case mtWaitAck, mtPaging:
		if x.sent >= w.s.cfg.MaxRetrans {
			uilog.Event(uilog.Orange, "SMS-NAS", "to %s: the phone did not acknowledge (ti %d)", uilog.Mask(w.supi), x.ti)
			w.finishMT(route.Failed(tpdu.RPCauseDestOutOfOrder))
			return
		}
		x.sent++
		w.sendMT()
	}
}

// mtUplink handles the phone's answers in an SMSF transaction.
func (w *worker) mtUplink(m cp.Msg) {
	x := w.mt
	if x == nil || x.ti != m.TI {
		switch {
		case m.Type == cp.TypeError:
			// never answer an error with an error
		case m.Type == cp.TypeData && int(m.TI) == w.lastTI:
			// a late or repeated report for a transaction already closed: acknowledge it so the phone stops
			w.n1(cp.EncodeAck(false, m.TI), w.idle(), "")
		default:
			w.n1(cp.EncodeError(false, m.TI, cp.CauseInvalidTI), false, "")
		}
		return
	}
	switch m.Type {
	case cp.TypeAck:
		if x.state != mtWaitData {
			stopTimer(x.timer)
			x.state = mtWaitData
			x.req.acked.Store(true)
		}
	case cp.TypeData:
		// the phone's RP report; it also stands in for a CP-ACK that was lost
		x.req.acked.Store(true)
		ok, _, cause, err := tpdu.DecodeRPReport(m.RPDU)
		a := route.Delivered
		switch {
		case err != nil:
			uilog.Event(uilog.Orange, "SMS-NAS", "to %s: the phone's report is unreadable", uilog.Mask(w.supi))
			a = route.Failed(tpdu.RPCauseRejected)
		case !ok:
			uilog.Event(uilog.Orange, "SMS-NAS", "to %s: the phone refused the sms, rp cause %d", uilog.Mask(w.supi), cause)
			a = route.Failed(tpdu.RPCauseRejected)
		}
		w.finishMT(a)
		w.n1(cp.EncodeAck(false, x.ti), w.idle(), "") // before the next MT's CP-DATA
	case cp.TypeError:
		uilog.Event(uilog.Orange, "SMS-NAS", "to %s: the phone ended the sms with cp cause %d", uilog.Mask(w.supi), m.Cause)
		w.finishMT(route.Failed(tpdu.RPCauseDestOutOfOrder))
	}
}

// idle says nothing else is going on with the phone, so the AMF may treat this N1 message as the last one.
func (w *worker) idle() bool { return w.mt == nil && len(w.queue) == 0 && len(w.mo) == 0 }

// mtDeadline: the router's deadline for the current MT ran out. A phone that took the CP-DATA most likely has the
// message (see Deliver); either way the next message may go.
func (w *worker) mtDeadline() {
	if w.mt.req.acked.Load() {
		w.finishMT(route.Delivered)
	} else {
		w.finishMT(route.Failed(tpdu.RPCauseTemporaryFailure))
	}
}

// finishMT reports the current MT's result and closes it; run starts the next one after the event.
func (w *worker) finishMT(a route.Attempt) {
	x := w.mt
	if x == nil {
		return
	}
	stopTimer(x.timer)
	w.forget(x.id)
	x.req.done <- a
	w.lastTI = int(x.ti)
	w.mt = nil
}
