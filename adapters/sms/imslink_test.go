// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package sms

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coranlabs/SETU/sms/route"
	"github.com/coranlabs/SETU/sms/tpdu"
)

const domain = "ims.mnc001.mcc001.3gppnetwork.org"

// sipReq is one MESSAGE the fake S-CSCF received.
type sipReq struct {
	ruri, from, branch, via string
	body                    []byte
}

// sipPeer is a UDP S-CSCF: it records each MESSAGE and answers it at its Via address with the code answer gives
// (0 = no answer).
type sipPeer struct {
	conn   net.PacketConn
	mu     sync.Mutex
	reqs   []sipReq
	answer func(sipReq) int
	got    chan sipReq
}

func newSIPPeer(t *testing.T, answer func(sipReq) int) *sipPeer {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &sipPeer{conn: conn, answer: answer, got: make(chan sipReq, 64)}
	t.Cleanup(func() { conn.Close() })
	go f.serve()
	return f
}

func (f *sipPeer) addr() string { return f.conn.LocalAddr().String() }

func (f *sipPeer) serve() {
	buf := make([]byte, 64<<10)
	for {
		n, _, err := f.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		head, body, _ := bytes.Cut(append([]byte(nil), buf[:n]...), []byte("\r\n\r\n"))
		var r sipReq
		r.body = body
		for i, ln := range strings.Split(string(head), "\r\n") {
			if i == 0 {
				if f := strings.Fields(ln); len(f) > 1 {
					r.ruri = f[1]
				}
				continue
			}
			k, v, _ := strings.Cut(ln, ": ")
			switch k {
			case "Via":
				r.via = v
				_, r.branch, _ = strings.Cut(v, ";branch=")
			case "From":
				r.from, _, _ = strings.Cut(v, ";")
			}
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, r)
		f.mu.Unlock()
		f.got <- r
		if code := f.answer(r); code != 0 {
			f.reply(r, code)
		}
	}
}

func (f *sipPeer) reply(r sipReq, code int) {
	hostport := strings.TrimPrefix(strings.SplitN(r.via, ";", 2)[0], "SIP/2.0/UDP ")
	to, err := net.ResolveUDPAddr("udp", hostport)
	if err != nil {
		return
	}
	resp := fmt.Sprintf("SIP/2.0 %d Whatever\r\nVia: %s\r\nCSeq: 1 MESSAGE\r\nContent-Length: 0\r\n\r\n", code, r.via)
	_, _ = f.conn.WriteTo([]byte(resp), to)
}

func (f *sipPeer) next(t *testing.T) sipReq {
	t.Helper()
	select {
	case r := <-f.got:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("the S-CSCF got no MESSAGE")
		return sipReq{}
	}
}

func startLink(t *testing.T, scscf string) *IMSLink {
	t.Helper()
	l := &IMSLink{SCSCF: scscf, SelfIP: "127.0.0.1", Domain: domain, Answer: 300 * time.Millisecond}
	if err := l.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// moHex is the RP-DATA a phone posts through the S-CSCF hook: "Hello" to 0100007488, ref 9.
func moHex() string {
	ud, septets := tpdu.GSM7Pack("Hello")
	submit := []byte{0x01, 0x00, 10, 0x91}
	submit = append(submit, tpdu.EncodeBCDAddr("0100007488")...)
	submit = append(submit, 0x00, 0x00, byte(septets))
	submit = append(submit, ud...)
	rp := append([]byte{0x00, 0x09, 0x00, 0x02, 0x91, 0xf7, byte(len(submit))}, submit...)
	return hex.EncodeToString(rp)
}

func postMO(a *Adapter, tpduHex string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/mo", strings.NewReader(`{"from":"+0100007487","tpdu":"`+tpduHex+`"}`))
	a.HandleMO(rec, req)
	return rec
}

// blockingRouter holds every Route until release is closed, then answers out.
type blockingRouter struct {
	release chan struct{}
	out     route.Outcome
	started chan struct{}
}

func (b *blockingRouter) Route(ctx context.Context, sender string, mo *tpdu.MO) route.Outcome {
	close(b.started)
	<-b.release
	return b.out
}

// /mo answers while routing is still running: the S-CSCF's worker is not held for the delivery
func TestHandleMOAnswersBeforeRouting(t *testing.T) {
	sc := newSIPPeer(t, func(sipReq) int { return 0 })
	br := &blockingRouter{release: make(chan struct{}), started: make(chan struct{}), out: route.Outcome{Delivered: true}}
	a := &Adapter{Domain: domain, Link: startLink(t, sc.addr()), Router: br}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postMO(a, moHex()) }()
	select {
	case rec := <-done:
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{}" {
			t.Fatalf("answer %d %q", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/mo waited for routing")
	}
	<-br.started
	close(br.release)
	r := sc.next(t)
	if r.ruri != "sip:0100007487@"+domain || !bytes.Equal(r.body, []byte{0x03, 0x09}) {
		t.Fatalf("report %s % x, want RP-ACK 03 09 to the sender", r.ruri, r.body)
	}
}

// the sender's report says what happened: RP-ACK when delivered, RP-ERROR with the cause when not
func TestReportReachesTheSender(t *testing.T) {
	for _, c := range []struct {
		out  route.Outcome
		want []byte
	}{
		{route.Outcome{Delivered: true, Domain: "ims"}, []byte{0x03, 0x09}},
		{route.Outcome{Cause: tpdu.RPCauseDestOutOfOrder}, []byte{0x05, 0x09, 0x01, 0x1b}},
	} {
		sc := newSIPPeer(t, func(sipReq) int { return 200 })
		br := &blockingRouter{release: make(chan struct{}), started: make(chan struct{}), out: c.out}
		close(br.release)
		a := &Adapter{Domain: domain, Link: startLink(t, sc.addr()), Router: br}
		postMO(a, moHex())
		r := sc.next(t)
		if r.ruri != "sip:0100007487@"+domain || r.from != "<sip:0100007488@"+domain+">" || !bytes.Equal(r.body, c.want) {
			t.Errorf("%+v: report to %s from %s: % x, want % x", c.out, r.ruri, r.from, r.body, c.want)
		}
	}
}

func TestSMMAGetsRPAck(t *testing.T) {
	sc := newSIPPeer(t, func(sipReq) int { return 200 })
	a := &Adapter{Domain: domain, SMSC: "7", Link: startLink(t, sc.addr()), Router: &blockingRouter{}}
	if rec := postMO(a, "0605"); strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("answer %q", rec.Body.String())
	}
	r := sc.next(t)
	if r.ruri != "sip:0100007487@"+domain || r.from != "<sip:7@"+domain+">" || !bytes.Equal(r.body, []byte{0x03, 0x05}) {
		t.Fatalf("SMMA answer to %s from %s: % x", r.ruri, r.from, r.body)
	}
}

func TestHandleMORefusesBadInput(t *testing.T) {
	a := &Adapter{Domain: domain, Link: &IMSLink{}, Router: &blockingRouter{}}
	for _, in := range []string{"zz", "", "0207", "000900"} {
		if rec := postMO(a, in); !strings.Contains(rec.Body.String(), `"err"`) {
			t.Errorf("%q: answer %q, want an error", in, rec.Body.String())
		}
	}
}

// the MESSAGE is byte for byte today's: Via at SelfIP:ViaPort, the relay mark, the 3GPP SMS body
func TestIMSLinkMessageShape(t *testing.T) {
	l := &IMSLink{SelfIP: "192.0.2.1", ViaPort: 8091, Domain: domain}
	m := string(l.message("0100007487", "0100007488", "z9hG4bKsms42", []byte{0x01, 0x02}))
	for _, want := range []string{
		"MESSAGE sip:0100007488@" + domain + " SIP/2.0\r\n",
		"\r\nVia: SIP/2.0/UDP 192.0.2.1:8091;branch=z9hG4bKsms42\r\nMax-Forwards: 70\r\nX-SMS-Relay: mt\r\nFrom: <sip:0100007487@" + domain + ">;tag=",
		"\r\nTo: <sip:0100007488@" + domain + ">\r\nCall-ID: sms",
		"@192.0.2.1\r\nCSeq: 1 MESSAGE\r\nP-Asserted-Identity: <sip:0100007487@" + domain + ">\r\nContent-Type: application/vnd.3gpp.sms\r\nContent-Length: 2\r\n\r\n\x01\x02",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q:\n%s", want, m)
		}
	}
}

func TestIMSLinkDeliver(t *testing.T) {
	for _, c := range []struct {
		name string
		code int
		want route.Attempt
	}{
		{"200", 200, route.Delivered},
		{"202", 202, route.Delivered},
		{"404 not registered", 404, route.Failed(27)},
		{"480", 480, route.Failed(27)},
		{"603", 603, route.Failed(27)},
		{"silence", 0, route.Unknown},
	} {
		sc := newSIPPeer(t, func(sipReq) int { return c.code })
		l := startLink(t, sc.addr())
		ctx := context.Background()
		if got := l.Deliver(ctx, "0100007488", []byte{0x01}); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// many deliveries at once: each gets the answer to its own MESSAGE, matched by the Via branch
func TestIMSLinkDemuxesByBranch(t *testing.T) {
	const n = 12
	sc := newSIPPeer(t, func(sipReq) int { return 0 })
	l := startLink(t, sc.addr())
	l.Answer = 3 * time.Second
	var wg sync.WaitGroup
	got := make([]route.Attempt, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = l.Deliver(context.Background(), fmt.Sprintf("01000074%02d%d", i, i%2), []byte{0x01})
		}()
	}
	// hold every MESSAGE, then answer them last first: odd recipients refuse
	held := make([]sipReq, 0, n)
	for range n {
		held = append(held, sc.next(t))
	}
	for i := len(held) - 1; i >= 0; i-- {
		code := 200
		if strings.HasSuffix(held[i].ruri, "1@"+domain) {
			code = 486
		}
		sc.reply(held[i], code)
	}
	wg.Wait()
	for i, a := range got {
		want := route.Delivered
		if i%2 == 1 {
			want = route.Failed(27)
		}
		if a != want {
			t.Errorf("delivery %d: %+v, want %+v", i, a, want)
		}
	}
}

// without the answer socket the link sends blind and cannot know: Unknown
func TestIMSLinkBlind(t *testing.T) {
	sc := newSIPPeer(t, func(sipReq) int { return 404 })
	l := &IMSLink{SCSCF: sc.addr(), SelfIP: "127.0.0.1", ViaPort: 9, Domain: domain}
	if got := l.Deliver(route.WithSender(context.Background(), "0100007487"), "0100007488", []byte{0x01}); got != route.Unknown {
		t.Fatalf("blind delivery %+v, want Unknown", got)
	}
	if r := sc.next(t); r.from != "<sip:0100007487@"+domain+">" {
		t.Fatalf("blind MESSAGE from %s", r.from)
	}
	// a socket that cannot be bound leaves the link blind
	busy, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer busy.Close()
	l2 := &IMSLink{SCSCF: sc.addr(), SelfIP: "127.0.0.1", ViaPort: busy.LocalAddr().(*net.UDPAddr).Port, Domain: domain}
	if err := l2.Start(); err == nil {
		t.Fatal("bound a port in use")
	}
	if got := l2.Deliver(context.Background(), "0100007488", []byte{0x01}); got != route.Unknown {
		t.Fatalf("delivery after a failed bind %+v, want Unknown", got)
	}
}

// lockedBuf collects log output from several goroutines.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// a whole MO through the real router: the log shows masked numbers and a count, never the text or a full number,
// and only ASCII
func TestLogsCarryNoTextOrNumber(t *testing.T) {
	var buf lockedBuf
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	sc := newSIPPeer(t, func(sipReq) int { return 200 })
	link := startLink(t, sc.addr())
	a := &Adapter{Domain: domain, Link: link, Router: &route.Router{IMS: link, SMSC: "7"}}
	postMO(a, moHex())
	sc.next(t) // the DELIVER
	sc.next(t) // the RP-ACK
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(buf.String(), "SMS-ACK") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	out := buf.String()
	if !strings.Contains(out, "***7487") || !strings.Contains(out, "delivered over ims") {
		t.Fatalf("log lacks the masked sender or the outcome:\n%s", out)
	}
	for _, bad := range []string{"Hello", "0100007487", "0100007488"} {
		if strings.Contains(out, bad) {
			t.Errorf("log carries %q:\n%s", bad, out)
		}
	}
	for i := 0; i < len(out); i++ {
		if out[i] >= 0x80 {
			t.Fatalf("log has a non-ASCII byte at %d:\n%s", i, out)
		}
	}
}

// An answer from any address but the S-CSCF's is dropped: a forged 200 must not mark a refused message delivered.
func TestIMSLinkIgnoresAnswersFromOtherHosts(t *testing.T) {
	sc := newSIPPeer(t, func(sipReq) int { return 0 })
	l := startLink(t, sc.addr())
	l.Answer = 3 * time.Second
	done := make(chan route.Attempt, 1)
	go func() { done <- l.Deliver(context.Background(), "0100007488", []byte{0x01}) }()
	r := sc.next(t)

	forger, err := net.ListenPacket("udp", "127.0.0.2:0")
	if err != nil {
		t.Skip("no 127.0.0.2 on this host:", err)
	}
	defer forger.Close()
	hostport := strings.TrimPrefix(strings.SplitN(r.via, ";", 2)[0], "SIP/2.0/UDP ")
	to, _ := net.ResolveUDPAddr("udp", hostport)
	forged := fmt.Sprintf("SIP/2.0 200 OK\r\nVia: %s\r\nCSeq: 1 MESSAGE\r\nContent-Length: 0\r\n\r\n", r.via)
	if _, err := forger.WriteTo([]byte(forged), to); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the forged answer is read (and dropped) first
	sc.reply(r, 486)

	if got := <-done; got != route.Failed(27) {
		t.Fatalf("got %+v, want the S-CSCF's refusal (rp cause 27), not the forged 200", got)
	}
}
