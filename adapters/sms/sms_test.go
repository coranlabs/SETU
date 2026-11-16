// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package sms

import (
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coranlabs/SETU/sms/tpdu"
)

// fakeSCSCF listens on UDP and collects the datagrams the adapter sends it.
type fakeSCSCF struct {
	conn *net.UDPConn
	msgs chan string
}

func newFakeSCSCF(t *testing.T) *fakeSCSCF {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSCSCF{conn: conn, msgs: make(chan string, 8)}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			f.msgs <- string(buf[:n])
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return f
}

func (f *fakeSCSCF) addr() string { return f.conn.LocalAddr().String() }

func (f *fakeSCSCF) next(t *testing.T) string {
	t.Helper()
	select {
	case m := <-f.msgs:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a SIP MESSAGE")
		return ""
	}
}

// submitRP builds the ms->n RP-DATA an S-CSCF posts for a mobile-originated SMS,
// picking the encoding a handset would: GSM 7-bit when the text fits, UCS-2
// otherwise.
func submitRP(t *testing.T, to, text string, ref byte) string {
	t.Helper()
	sub := []byte{0x01, 0x00, byte(len(to)), 0x91}
	sub = append(sub, tpdu.EncodeBCDAddr(to)...)
	sub = append(sub, 0x00) // TP-PID
	if tpdu.IsGSM7(text) {
		ud, septets := tpdu.GSM7Pack(text)
		sub = append(sub, 0x00, byte(septets)) // TP-DCS, TP-UDL
		sub = append(sub, ud...)
	} else {
		ud := tpdu.UCS2Encode(text)
		sub = append(sub, 0x08, byte(len(ud))) // TP-DCS, TP-UDL
		sub = append(sub, ud...)
	}
	rp := []byte{0x00, ref, 0x00, 0x02, 0x91, 0xf7, byte(len(sub))}
	rp = append(rp, sub...)
	return hex.EncodeToString(rp)
}

func post(t *testing.T, a *Adapter, from, tpduHex string) moResp {
	t.Helper()
	body, _ := json.Marshal(moReq{From: from, TPDU: tpduHex})
	r := httptest.NewRequest(http.MethodPost, "/mo", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	a.HandleMO(w, r)
	var out moResp
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func newAdapter(scscf string) *Adapter {
	return &Adapter{
		SCSCF: scscf, SelfIP: "127.0.0.1", ViaPort: 8091,
		Domain: "ims.mnc001.mcc001.3gppnetwork.org",
		Now:    func() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) },
	}
}

// A mobile-originated message must produce a terminating MESSAGE towards the
// recipient followed by a submit report back to the sender.
func TestMOProducesDeliverThenSubmitReport(t *testing.T) {
	scscf := newFakeSCSCF(t)
	a := newAdapter(scscf.addr())

	out := post(t, a, "+1001001489", submitRP(t, "1001001488", "Hi there", 7))
	if out.Err != "" {
		t.Fatalf("unexpected error: %s", out.Err)
	}

	mt := scscf.next(t)
	if !strings.HasPrefix(mt, "MESSAGE sip:1001001488@ims.mnc001.mcc001.3gppnetwork.org SIP/2.0") {
		t.Fatalf("bad request line: %q", strings.SplitN(mt, "\r\n", 2)[0])
	}
	for _, want := range []string{
		"X-SMS-Relay: mt",
		"Content-Type: application/vnd.3gpp.sms",
		"P-Asserted-Identity: <sip:1001001489@ims.mnc001.mcc001.3gppnetwork.org>",
		"CSeq: 1 MESSAGE",
	} {
		if !strings.Contains(mt, want) {
			t.Errorf("MT message missing %q", want)
		}
	}
	if got := deliveredText(t, []byte(mt[strings.Index(mt, "\r\n\r\n")+4:])); got != "Hi there" {
		t.Errorf("MT body carries %q, want %q", got, "Hi there")
	}

	ack := scscf.next(t)
	if !strings.Contains(ack, "sip:1001001489@") {
		t.Error("submit report was not addressed to the sender")
	}
	if got := []byte(ack[strings.Index(ack, "\r\n\r\n")+4:]); len(got) != 2 || got[0] != 0x03 || got[1] != 7 {
		t.Errorf("submit report = % x, want 03 07", got)
	}
}

// The MT body must round-trip back to the original text, including UCS-2.
func TestMOEncodings(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"gsm7", "Plain text 123"},
		{"ucs2", "emoji 🤗 ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scscf := newFakeSCSCF(t)
			a := newAdapter(scscf.addr())

			out := post(t, a, "1001001489", submitRP(t, "1001001488", tc.text, 1))
			if out.Err != "" {
				t.Fatalf("error: %s", out.Err)
			}
			mt := scscf.next(t)
			raw := []byte(mt[strings.Index(mt, "\r\n\r\n")+4:])
			if got := deliveredText(t, raw); got != tc.text {
				t.Fatalf("delivered %q, want %q", got, tc.text)
			}
			deliver := tpduOf(raw)
			wantDCS := byte(0x00)
			if !tpdu.IsGSM7(tc.text) {
				wantDCS = 0x08
			}
			if got := deliver[9]; got != wantDCS {
				t.Errorf("TP-DCS = %#x, want %#x", got, wantDCS)
			}
		})
	}
}

func TestMOBadInput(t *testing.T) {
	scscf := newFakeSCSCF(t)
	a := newAdapter(scscf.addr())

	for _, tc := range []struct{ name, tpduHex, wantPrefix string }{
		{"not hex", "zzzz", "bad hex"},
		{"truncated rp", "0001", "decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := post(t, a, "1001001489", tc.tpduHex)
			if !strings.HasPrefix(out.Err, tc.wantPrefix) {
				t.Fatalf("err = %q, want prefix %q", out.Err, tc.wantPrefix)
			}
		})
	}
}

// A dead S-CSCF must be reported, not panic or hang.
func TestMOUnreachableSCSCF(t *testing.T) {
	a := newAdapter("127.0.0.1:1") // nothing listens here
	out := post(t, a, "1001001489", submitRP(t, "1001001488", "hi", 2))
	// /mo answers before delivery starts (the sender learns of a failed delivery from its RP-ERROR or the missing
	// RP-ACK), so even with a dead S-CSCF the answer comes back at once and carries no error.
	if out.Err != "" {
		t.Fatalf("err = %q, want none: delivery is asynchronous", out.Err)
	}
}

// tpduOf is the TPDU inside an RP-DATA n->ms: type, reference, RP-OA, RP-DA, then its length (TS 24.011 7.3.1.1).
func tpduOf(rp []byte) []byte {
	i := 2
	i += 1 + int(rp[i]) // RP-OA
	i += 1 + int(rp[i]) // RP-DA
	return rp[i+1:]
}

// deliveredText is the text of the SMS-DELIVER inside an RP-DATA n->ms (TS 23.040 9.2.2.1).
func deliveredText(t *testing.T, rp []byte) string {
	t.Helper()
	d := tpduOf(rp)
	oa := (int(d[1]) + 1) / 2
	dcs := d[3+oa+1]
	udl := int(d[3+oa+2+7])
	ud := d[3+oa+2+7+1:]
	if dcs&0x0c == 0x08 {
		return tpdu.DecodeUCS2(ud[:udl])
	}
	return tpdu.GSM7Unpack(ud, udl)
}
