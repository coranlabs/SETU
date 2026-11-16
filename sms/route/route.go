// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package route is setu's one SMS router: a message sent over IMS (the S-CSCF's /mo hook) or over NAS (the SMSF's
// sendsms) is delivered to the recipient over whichever domain reaches them, and the sender gets an RP-ACK or an
// RP-ERROR that says what really happened. Before it, the IMS path sent RP-ACK even when delivery failed.
package route

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coranlabs/SETU/sms/tpdu"
	"github.com/coranlabs/SETU/uilog"
)

// Status is how one delivery attempt ended.
type Status uint8

const (
	// StatusUnknown: the message went out and no answer said yes or no (IMS without the S-CSCF's answer). It counts
	// as delivered, which is what the IMS path always assumed.
	StatusUnknown Status = iota
	StatusDelivered
	StatusFailed
)

// Attempt is the result of one delivery attempt over one domain. Cause is the RP-Cause when Status is StatusFailed.
type Attempt struct {
	Status Status
	Cause  byte
}

// The attempt values; Failed carries its RP-Cause.
var (
	Delivered = Attempt{Status: StatusDelivered}
	Unknown   = Attempt{Status: StatusUnknown}
)

// Failed is a failed attempt with the RP-Cause the sender would see.
func Failed(cause byte) Attempt { return Attempt{Status: StatusFailed, Cause: cause} }

// Deliverer sends an n->ms RP-DATA to the recipient over one domain.
type Deliverer interface {
	Deliver(ctx context.Context, recipient string, rpData []byte) Attempt
}

// Directory answers whether a number can be reached over NAS (an SMSF context exists for exactly one SIM with it).
type Directory interface {
	Reachable(msisdn string) bool
}

// The two orders: nas-first suits phones that take SMS over NAS (SMS allowed in their Registration Accept), ims-first
// the IMS-only deployments that ran before setu had an SMSF.
const (
	OrderNASFirst = "nas-first"
	OrderIMSFirst = "ims-first"
)

// DefaultDeadline bounds a whole route when Router.Deadline is 0: it keeps the RP report inside the phone's TR1M
// (35-45 s, TS 24.011 Table 10.1) with time left for the CP retransmission.
const DefaultDeadline = 25 * time.Second

// Router delivers MO messages. IMS and NAS may each be nil (that domain is not tried); Dir is required for NAS.
type Router struct {
	IMS, NAS Deliverer
	Dir      Directory
	Order    string        // OrderNASFirst (the default) or OrderIMSFirst
	Deadline time.Duration // whole route, both domains; DefaultDeadline when 0
	SMSC     string        // the SC address carried in RP-OA of the DELIVER
	Now      func() time.Time

	mr atomic.Uint32 // the next RP-Message-Reference for an MT RP-DATA
}

// Outcome is what the sender is told.
type Outcome struct {
	Delivered bool
	Domain    string // "ims" or "nas" when delivered
	Cause     byte   // RP-Cause when not delivered
}

// Report is the n->ms RP message answering the sender's RP-DATA with reference ref.
func (o Outcome) Report(ref byte) []byte {
	if o.Delivered {
		return tpdu.BuildRPAck(ref)
	}
	return tpdu.BuildRPError(ref, o.Cause)
}

type senderKey struct{}

// WithSender carries the MO sender's number to the Deliverers. The IMS side puts it in the MT MESSAGE's From and
// P-Asserted-Identity, as it always has; the RP-DATA alone does not give it to the S-CSCF.
func WithSender(ctx context.Context, sender string) context.Context {
	return context.WithValue(ctx, senderKey{}, sender)
}

// Sender is the number WithSender put in ctx, or "".
func Sender(ctx context.Context) string {
	s, _ := ctx.Value(senderKey{}).(string)
	return s
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Route delivers mo from sender and says what to tell the sender. It never takes longer than the deadline.
func (r *Router) Route(ctx context.Context, sender string, mo *tpdu.MO) Outcome {
	if sender == "" {
		uilog.Event(uilog.Red, "SMS-FAIL", "to %s: the sender has no number", uilog.Mask(mo.Recipient))
		return Outcome{Cause: tpdu.RPCauseNetworkOutOfOrder}
	}
	d := r.Deadline
	if d <= 0 {
		d = DefaultDeadline
	}
	ctx, cancel := context.WithTimeout(WithSender(ctx, sender), d)
	defer cancel()
	rp := tpdu.EncodeDeliverWith(sender, mo.Text, r.now(), r.SMSC, byte(r.mr.Add(1)))

	nasOK := r.NAS != nil && r.Dir != nil && r.Dir.Reachable(mo.Recipient)
	type leg struct {
		name string
		d    Deliverer
	}
	var legs []leg
	ims := leg{"ims", r.IMS}
	nas := leg{"nas", r.NAS}
	switch {
	case r.Order == OrderIMSFirst && nasOK:
		legs = []leg{ims, nas}
	case r.Order == OrderIMSFirst:
		legs = []leg{ims}
	case nasOK:
		legs = []leg{nas, ims}
	default:
		legs = []leg{ims}
	}

	out := Outcome{Cause: tpdu.RPCauseDestOutOfOrder}
	for _, l := range legs {
		if l.d == nil {
			continue
		}
		if ctx.Err() != nil {
			out = Outcome{Cause: tpdu.RPCauseTemporaryFailure}
			break
		}
		a := l.d.Deliver(ctx, mo.Recipient, rp)
		if a.Status != StatusFailed {
			if a.Status == StatusDelivered || ctx.Err() == nil {
				out = Outcome{Delivered: true, Domain: l.name}
				break
			}
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			out = Outcome{Cause: tpdu.RPCauseTemporaryFailure}
			break
		}
		// "unreachable" is the least telling cause: any other one (the recipient refused, the core failed) is kept
		if out.Cause == tpdu.RPCauseDestOutOfOrder && a.Cause != 0 {
			out.Cause = a.Cause
		}
	}

	chars := utf8.RuneCountInString(mo.Text)
	if out.Delivered {
		uilog.Event(uilog.Yellow, "SMS", "%s to %s  %d chars  delivered over %s", uilog.Mask(sender), uilog.Mask(mo.Recipient), chars, out.Domain)
	} else {
		uilog.Event(uilog.Red, "SMS-FAIL", "%s to %s  %d chars  not delivered, rp cause %d", uilog.Mask(sender), uilog.Mask(mo.Recipient), chars, out.Cause)
	}
	return out
}
