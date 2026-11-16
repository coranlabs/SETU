// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package sms

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coranlabs/SETU/sms/route"
	"github.com/coranlabs/SETU/sms/tpdu"
)

// DefaultIMSAnswer is how long Deliver waits for the S-CSCF's final answer when IMSLink.Answer is 0.
const DefaultIMSAnswer = 4 * time.Second

// IMSLink is setu's SIP side for MT messages. It sends from one UDP socket bound to SelfIP:ViaPort, the address the
// Via header has always advertised, so the S-CSCF's answer (which goes to the Via address: there is no rport) now
// reaches setu and says whether the recipient took the message. Without the socket (the bind failed) it sends
// blind, as setu always did, and every delivery is Unknown.
type IMSLink struct {
	SCSCF   string // host:port (UDP SIP)
	SelfIP  string // advertised in Via
	ViaPort int    // advertised Via port, and the port answers come back to
	Domain  string
	Answer  time.Duration // DefaultIMSAnswer when 0

	conn    net.PacketConn // nil: blind
	peer    net.IP         // the S-CSCF's IP: answers from any other address are dropped
	mu      sync.Mutex
	waiters map[string]chan int // top Via branch -> final status code
}

// Start binds the answer socket and reads answers until Close. When the bind fails it logs and returns the error;
// the link keeps working blind.
func (l *IMSLink) Start() error {
	addr := net.JoinHostPort(l.SelfIP, strconv.Itoa(l.ViaPort))
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Printf("sms: cannot listen on %s for S-CSCF answers (%v): MT delivery stays blind", addr, err)
		return err
	}
	if l.ViaPort == 0 {
		l.ViaPort = conn.LocalAddr().(*net.UDPAddr).Port
	}
	var peer net.IP
	if a, err := net.ResolveUDPAddr("udp", l.SCSCF); err == nil {
		peer = a.IP
	} else {
		log.Printf("sms: cannot resolve the S-CSCF %q (%v): its answers are not trusted, MT delivery stays unconfirmed", l.SCSCF, err)
	}
	l.mu.Lock()
	l.conn = conn
	l.peer = peer
	l.waiters = map[string]chan int{}
	l.mu.Unlock()
	log.Printf("sms: S-CSCF answers on %s", conn.LocalAddr())
	go l.read(conn)
	return nil
}

// Close stops the answer socket.
func (l *IMSLink) Close() error {
	l.mu.Lock()
	conn := l.conn
	l.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

func (l *IMSLink) socket() net.PacketConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn
}

// read hands every final answer (200 and above) to the Deliver waiting on its branch. Requests, provisional answers
// and answers nobody waits for (the RP-ACK and RP-ERROR reports) are dropped.
func (l *IMSLink) read(conn net.PacketConn) {
	buf := make([]byte, 64<<10)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if !l.fromSCSCF(from) {
			continue // only the S-CSCF may say whether a message was delivered
		}
		code, branch, ok := parseAnswer(buf[:n])
		if !ok || code < 200 {
			continue
		}
		l.mu.Lock()
		ch := l.waiters[branch]
		l.mu.Unlock()
		if ch != nil {
			select {
			case ch <- code:
			default: // a retransmitted answer: the first one counts
			}
		}
	}
}

// fromSCSCF says whether a datagram came from the S-CSCF's IP (its port may differ: Kamailio can answer from any of
// its sockets).
func (l *IMSLink) fromSCSCF(from net.Addr) bool {
	u, ok := from.(*net.UDPAddr)
	if !ok {
		return false
	}
	l.mu.Lock()
	peer := l.peer
	l.mu.Unlock()
	return peer != nil && peer.Equal(u.IP)
}

// parseAnswer reads a SIP answer's status code and its top Via branch.
func parseAnswer(b []byte) (code int, branch string, ok bool) {
	head, _, _ := bytes.Cut(b, []byte("\r\n\r\n"))
	lines := strings.Split(string(head), "\r\n")
	f := strings.Fields(lines[0])
	if len(f) < 2 || f[0] != "SIP/2.0" {
		return 0, "", false
	}
	code, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, "", false
	}
	for _, ln := range lines[1:] {
		name, val, found := strings.Cut(ln, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if !strings.EqualFold(name, "Via") && !strings.EqualFold(name, "v") {
			continue
		}
		top, _, _ := strings.Cut(val, ",") // several Vias may share one line; the first is the top one
		for _, p := range strings.Split(top, ";")[1:] {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			if strings.EqualFold(k, "branch") {
				return code, strings.TrimSpace(v), true
			}
		}
		return code, "", true
	}
	return code, "", true
}

// message builds the MT SIP MESSAGE (application/vnd.3gpp.sms) for the S-CSCF, marked X-SMS-Relay: mt so the
// S-CSCF's MT hook routes it terminating without re-interception. Header layout preserved from the proven system.
func (l *IMSLink) message(sender, recipient, branch string, payload []byte) []byte {
	ruri := fmt.Sprintf("sip:%s@%s", recipient, l.Domain)
	rnd := func() int { return rand.IntN(1 << 30) }
	hdr := fmt.Sprintf("MESSAGE %s SIP/2.0\r\nVia: SIP/2.0/UDP %s:%d;branch=%s\r\nMax-Forwards: 70\r\nX-SMS-Relay: mt\r\nFrom: <sip:%s@%s>;tag=%d\r\nTo: <sip:%s@%s>\r\nCall-ID: sms%d%d@%s\r\nCSeq: 1 MESSAGE\r\nP-Asserted-Identity: <sip:%s@%s>\r\nContent-Type: application/vnd.3gpp.sms\r\nContent-Length: %d\r\n\r\n",
		ruri, l.SelfIP, l.ViaPort, branch, sender, l.Domain, rnd(), recipient, l.Domain, rnd(), rnd(), l.SelfIP, sender, l.Domain, len(payload))
	return append([]byte(hdr), payload...)
}

// newBranch picks a Via branch no waiting Deliver uses. Called with l.mu held.
func (l *IMSLink) newBranch() string {
	for {
		b := fmt.Sprintf("z9hG4bKsms%d", rand.IntN(1<<30))
		if _, used := l.waiters[b]; !used {
			return b
		}
	}
}

// Send sends one MT MESSAGE carrying payload from sender to recipient and does not wait for an answer.
func (l *IMSLink) Send(sender, recipient string, payload []byte) error {
	l.mu.Lock()
	branch := l.newBranch()
	l.mu.Unlock()
	return l.write(l.message(sender, recipient, branch, payload))
}

func (l *IMSLink) write(pkt []byte) error {
	if conn := l.socket(); conn != nil {
		addr, err := net.ResolveUDPAddr("udp", l.SCSCF)
		if err != nil {
			return err
		}
		_, err = conn.WriteTo(pkt, addr)
		return err
	}
	conn, err := net.Dial("udp", l.SCSCF)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(pkt)
	return err
}

// Deliver sends the RP-DATA to the recipient over IMS. A 2xx answer means the recipient took it; 4xx-6xx (the
// S-CSCF's 404 for a recipient not registered in IMS, or the phone's own refusal) means it did not; no answer in time
// is Unknown, which the router counts as delivered, like the blind path always did.
func (l *IMSLink) Deliver(ctx context.Context, recipient string, rpData []byte) route.Attempt {
	if l.SCSCF == "" {
		return route.Failed(tpdu.RPCauseDestOutOfOrder)
	}
	sender := route.Sender(ctx)
	if l.socket() == nil {
		if err := l.Send(sender, recipient, rpData); err != nil {
			log.Printf("sms: MT to the S-CSCF: %v", err)
			return route.Failed(tpdu.RPCauseNetworkOutOfOrder)
		}
		return route.Unknown
	}
	ch := make(chan int, 1)
	l.mu.Lock()
	branch := l.newBranch()
	l.waiters[branch] = ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.waiters, branch)
		l.mu.Unlock()
	}()
	if err := l.write(l.message(sender, recipient, branch, rpData)); err != nil {
		log.Printf("sms: MT to the S-CSCF: %v", err)
		return route.Failed(tpdu.RPCauseNetworkOutOfOrder)
	}
	wait := l.Answer
	if wait <= 0 {
		wait = DefaultIMSAnswer
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case code := <-ch:
		if code < 300 {
			return route.Delivered
		}
		return route.Failed(tpdu.RPCauseDestOutOfOrder)
	case <-t.C:
		return route.Unknown
	case <-ctx.Done():
		return route.Unknown
	}
}
