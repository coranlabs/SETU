// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coranlabs/SETU/sms/tpdu"
)

// fakeLeg is a Deliverer that records its calls and answers with a fixed attempt (or waits for the deadline).
type fakeLeg struct {
	mu    sync.Mutex
	calls []string // recipients
	rps   [][]byte
	from  []string
	ans   Attempt
	block bool // wait for ctx and answer Unknown
}

func (f *fakeLeg) Deliver(ctx context.Context, recipient string, rp []byte) Attempt {
	f.mu.Lock()
	f.calls = append(f.calls, recipient)
	f.rps = append(f.rps, rp)
	f.from = append(f.from, Sender(ctx))
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return Unknown
	}
	return f.ans
}

func (f *fakeLeg) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

// dir is a Directory listing the numbers with an SMSF context (an ambiguous number is simply not listed: the SMSF's
// Reachable answers false for it).
type dir map[string]bool

func (d dir) Reachable(m string) bool { return d[m] }

var mo = &tpdu.MO{Recipient: "0100007488", Text: "Hi", RPRef: 9}

func TestRoute(t *testing.T) {
	for _, c := range []struct {
		name       string
		order      string
		nas, ims   Attempt
		reach      dir
		want       Outcome
		nasN, imsN int
		withoutIMS bool
	}{
		{"nas-first with a context", "", Delivered, Delivered, dir{"0100007488": true}, Outcome{Delivered: true, Domain: "nas"}, 1, 0, false},
		{"nas failure falls back to ims", "", Failed(27), Delivered, dir{"0100007488": true}, Outcome{Delivered: true, Domain: "ims"}, 1, 1, false},
		{"no context goes to ims", "nas-first", Delivered, Delivered, dir{}, Outcome{Delivered: true, Domain: "ims"}, 0, 1, false},
		{"ims 404 gives 27", "", Delivered, Failed(27), dir{}, Outcome{Cause: 27}, 0, 1, false},
		{"ims unknown counts as delivered", "", Delivered, Unknown, dir{}, Outcome{Delivered: true, Domain: "ims"}, 0, 1, false},
		{"the recipient refusing beats unreachable", "", Failed(21), Failed(27), dir{"0100007488": true}, Outcome{Cause: 21}, 1, 1, false},
		{"ims-first tries ims first", "ims-first", Delivered, Delivered, dir{"0100007488": true}, Outcome{Delivered: true, Domain: "ims"}, 0, 1, false},
		{"ims-first falls back to nas", "ims-first", Delivered, Failed(27), dir{"0100007488": true}, Outcome{Delivered: true, Domain: "nas"}, 1, 1, false},
		{"ims-first without a context", "ims-first", Delivered, Failed(27), dir{}, Outcome{Cause: 27}, 0, 1, false},
		{"an ambiguous number is not sent over nas", "", Delivered, Failed(27), dir{"0100007488": false}, Outcome{Cause: 27}, 0, 1, false},
		{"no domain at all gives 27", "", Delivered, Delivered, dir{}, Outcome{Cause: 27}, 0, 0, true},
		{"the core failing gives 38", "", Failed(38), Failed(27), dir{"0100007488": true}, Outcome{Cause: 38}, 1, 1, false},
	} {
		nas, ims := &fakeLeg{ans: c.nas}, &fakeLeg{ans: c.ims}
		r := &Router{NAS: nas, IMS: ims, Dir: c.reach, Order: c.order, SMSC: "7"}
		if c.withoutIMS {
			r.IMS = nil
		}
		got := r.Route(context.Background(), "0100007487", mo)
		if got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
		if nas.n() != c.nasN || ims.n() != c.imsN {
			t.Errorf("%s: nas tried %d, ims %d, want %d and %d", c.name, nas.n(), ims.n(), c.nasN, c.imsN)
		}
	}
}

// the deadline bounds the whole route; when it runs out the sender gets 41 and no second domain is tried
func TestRouteDeadline(t *testing.T) {
	nas, ims := &fakeLeg{block: true}, &fakeLeg{ans: Delivered}
	r := &Router{NAS: nas, IMS: ims, Dir: dir{"0100007488": true}, Deadline: 50 * time.Millisecond}
	start := time.Now()
	got := r.Route(context.Background(), "0100007487", mo)
	if got != (Outcome{Cause: tpdu.RPCauseTemporaryFailure}) {
		t.Fatalf("outcome %+v, want cause 41", got)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("route took %v", el)
	}
	if ims.n() != 0 {
		t.Fatal("ims tried after the deadline")
	}
}

func TestRouteBuildsTheDeliver(t *testing.T) {
	frozen := time.Date(2025, 8, 6, 0, 0, 0, 0, time.UTC)
	ims := &fakeLeg{ans: Delivered}
	r := &Router{IMS: ims, SMSC: "7", Now: func() time.Time { return frozen }}
	r.Route(context.Background(), "0100007487", mo)
	r.Route(context.Background(), "0100007487", mo)
	if ims.from[0] != "0100007487" {
		t.Fatalf("the deliverer saw sender %q", ims.from[0])
	}
	// the DELIVER is EncodeDeliverWith's, with a new RP-MR each time
	if want := tpdu.EncodeDeliverWith("0100007487", "Hi", frozen, "7", 1); !bytes.Equal(ims.rps[0], want) {
		t.Fatalf("first DELIVER % x, want % x", ims.rps[0], want)
	}
	if ims.rps[1][1] != 2 {
		t.Fatalf("second RP-MR %d, want 2", ims.rps[1][1])
	}
}

func TestRouteWithoutSenderNumber(t *testing.T) {
	ims := &fakeLeg{ans: Delivered}
	if got := (&Router{IMS: ims}).Route(context.Background(), "", mo); got != (Outcome{Cause: 38}) {
		t.Fatalf("outcome %+v, want cause 38", got)
	}
	if ims.n() != 0 {
		t.Fatal("delivered without a sender")
	}
}

func TestReport(t *testing.T) {
	if got := (Outcome{Delivered: true, Domain: "nas"}).Report(9); !bytes.Equal(got, []byte{0x03, 0x09}) {
		t.Fatalf("delivered report % x, want 03 09", got)
	}
	if got := (Outcome{Cause: 27}).Report(9); !bytes.Equal(got, []byte{0x05, 0x09, 0x01, 0x1b}) {
		t.Fatalf("failed report % x, want 05 09 01 1b", got)
	}
}
