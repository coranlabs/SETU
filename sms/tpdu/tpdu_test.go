// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package tpdu

import (
	"bytes"
	"testing"
	"time"
)

// frozen reproduces the proven system's hardcoded TP-SCTS bytes 52 80 60 00 00 00 00
// (= 2025-08-06 00:00:00, nibble-swapped BCD, tz 0).
var frozen = time.Date(2025, 8, 6, 0, 0, 0, 0, time.UTC)

func TestEncodeDeliverGolden(t *testing.T) {
	rp := EncodeDeliver("1001001489", "Hello", frozen)

	// RP layer: RP-DATA n->ms, ref 0, RP-OA = SMSC "7" (0x02 0x91 0xF7), empty RP-DA.
	wantPrefix := []byte{0x01, 0x00, 0x02, 0x91, 0xf7, 0x00}
	if !bytes.HasPrefix(rp, wantPrefix) {
		t.Fatalf("RP prefix = % x, want % x", rp[:6], wantPrefix)
	}
	tp := rp[7:] // RP-UD length byte, then the TPDU
	if int(rp[6]) != len(tp) {
		t.Fatalf("RP-UD length = %d, want %d", rp[6], len(tp))
	}
	// TPDU: SMS-DELIVER, sender 10 digits international, then PID/DCS/SCTS.
	if tp[0] != 0x04 {
		t.Fatalf("TP first octet = %#x, want 0x04 (SMS-DELIVER)", tp[0])
	}
	if tp[1] != 10 || tp[2] != 0x91 {
		t.Fatalf("TP-OA header = % x, want 0a 91", tp[1:3])
	}
	if got := DecodeBCDAddr(tp[3:8], 10); got != "1001001489" {
		t.Fatalf("TP-OA = %q, want 1001001489", got)
	}
	if tp[8] != 0x00 || tp[9] != 0x00 {
		t.Fatalf("PID/DCS = % x, want 00 00 (GSM-7)", tp[8:10])
	}
	wantSCTS := []byte{0x52, 0x80, 0x60, 0x00, 0x00, 0x00, 0x00}
	if !bytes.Equal(tp[10:17], wantSCTS) {
		t.Fatalf("TP-SCTS = % x, want % x (frozen 2025-08-06)", tp[10:17], wantSCTS)
	}
	udl := int(tp[17])
	if udl != 5 {
		t.Fatalf("UDL = %d, want 5", udl)
	}
	if got := GSM7Unpack(tp[18:], udl); got != "Hello" {
		t.Fatalf("UD = %q, want Hello", got)
	}
}

func TestEncodeDeliverUCS2Emoji(t *testing.T) {
	text := "hi 👍"
	rp := EncodeDeliver("1001001488", text, frozen)
	tp := rp[7:]
	if tp[9] != 0x08 {
		t.Fatalf("DCS = %#x, want 0x08 (UCS2)", tp[9])
	}
	udl := int(tp[17])
	if got := DecodeUCS2(tp[18 : 18+udl]); got != text {
		t.Fatalf("UD = %q, want %q", got, text)
	}
}

func TestDecodeMORoundTrip(t *testing.T) {
	// Build an ms->n RP-DATA + SMS-SUBMIT for "Hi" to 1001001488, ref 7 — the shape
	// a handset (via the S-CSCF hook) actually posts.
	ud, septets := GSM7Pack("Hi")
	submit := []byte{0x01, 0x00, 10, 0x91}
	submit = append(submit, EncodeBCDAddr("1001001488")...)
	submit = append(submit, 0x00, 0x00, byte(septets))
	submit = append(submit, ud...)
	rp := []byte{0x00, 0x07, 0x00, 0x02, 0x91, 0xf7, byte(len(submit))}
	rp = append(rp, submit...)

	mo, err := DecodeMO(rp)
	if err != nil {
		t.Fatal(err)
	}
	if mo.Recipient != "1001001488" || mo.Text != "Hi" || mo.RPRef != 7 {
		t.Fatalf("MO = %+v, want recipient 1001001488, text Hi, ref 7", mo)
	}
}

func TestBuildRPAck(t *testing.T) {
	if got := BuildRPAck(7); !bytes.Equal(got, []byte{0x03, 0x07}) {
		t.Fatalf("RP-ACK = % x, want 03 07", got)
	}
}

// moRP is the ms->n RP-DATA of TestDecodeMORoundTrip: "Hi" to 1001001488, ref 7.
func moRP() []byte {
	ud, septets := GSM7Pack("Hi")
	submit := []byte{0x01, 0x00, 10, 0x91}
	submit = append(submit, EncodeBCDAddr("1001001488")...)
	submit = append(submit, 0x00, 0x00, byte(septets))
	submit = append(submit, ud...)
	rp := []byte{0x00, 0x07, 0x00, 0x02, 0x91, 0xf7, byte(len(submit))}
	return append(rp, submit...)
}

// a message cut anywhere is an error, never a panic (the S-CSCF hook and the AMF both pass on whatever the phone sent)
func TestDecodeMOTruncated(t *testing.T) {
	full := moRP()
	for k := 0; k < len(full); k++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("DecodeMO(% x) panicked: %v", full[:k], r)
				}
			}()
			if mo, err := DecodeMO(full[:k]); err == nil {
				t.Errorf("DecodeMO(% x) accepted a cut message as %+v", full[:k], mo)
			}
		}()
	}
}

// a UCS-2 SUBMIT whose user data is cut, and one whose header claims more than is there
func TestDecodeMOUCS2AndHeaderBounds(t *testing.T) {
	ucs := UCS2Encode("hi 👍")
	submit := []byte{0x01, 0x00, 10, 0x91}
	submit = append(submit, EncodeBCDAddr("1001001488")...)
	submit = append(submit, 0x00, 0x08, byte(len(ucs)))
	submit = append(submit, ucs...)
	rp := append([]byte{0x00, 0x03, 0x00, 0x00, byte(len(submit))}, submit...)
	mo, err := DecodeMO(rp)
	if err != nil || mo.Text != "hi 👍" || mo.RPRef != 3 {
		t.Fatalf("UCS-2 = %+v, %v", mo, err)
	}
	if _, err := DecodeMO(rp[:len(rp)-1]); err == nil {
		t.Fatal("a UCS-2 SUBMIT one octet short was accepted")
	}
	// TP-UDHI set and a header length past the end of the user data
	bad := append([]byte(nil), submit...)
	bad[0] |= 0x40
	bad[len(bad)-len(ucs)] = 0xf0
	rpBad := append([]byte{0x00, 0x03, 0x00, 0x00, byte(len(bad))}, bad...)
	if _, err := DecodeMO(rpBad); err == nil {
		t.Fatal("a header longer than the user data was accepted")
	}
}

func TestDecodeMOChecksTheType(t *testing.T) {
	for _, mti := range []byte{RPDataN, RPAckMS, RPErrorMS, RPSMMA} {
		rp := moRP()
		rp[0] = mti
		if _, err := DecodeMO(rp); err != ErrNotRPData {
			t.Errorf("MTI %d: err %v, want ErrNotRPData", mti, err)
		}
	}
	if _, err := DecodeMO(moRP()); err != nil {
		t.Fatalf("MTI 0 refused: %v", err)
	}
}

func TestRPType(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want byte
	}{
		{[]byte{0x00, 0x01}, RPDataMS}, {[]byte{0x06, 0x09}, RPSMMA}, {[]byte{0x02, 0x00}, RPAckMS},
		{[]byte{0xf4, 0x00}, RPErrorMS}, // the spare bits above the MTI are ignored
	} {
		if got, err := RPType(c.in); err != nil || got != c.want {
			t.Errorf("RPType(% x) = %d, %v, want %d", c.in, got, err, c.want)
		}
	}
	if _, err := RPType(nil); err == nil {
		t.Fatal("empty message accepted")
	}
}

func TestBuildRPError(t *testing.T) {
	if got := BuildRPError(7, RPCauseDestOutOfOrder); !bytes.Equal(got, []byte{0x05, 0x07, 0x01, 0x1b}) {
		t.Fatalf("RP-ERROR = % x, want 05 07 01 1b", got)
	}
}

func TestDecodeRPReport(t *testing.T) {
	for _, c := range []struct {
		name      string
		in        []byte
		ok        bool
		ref, wcau byte
		err       bool
	}{
		{"bare RP-ACK", []byte{0x02, 0x05}, true, 5, 0, false},
		{"RP-ACK with a DELIVER-REPORT", []byte{0x02, 0x05, 0x41, 0x02, 0x00, 0x00}, true, 5, 0, false},
		{"RP-ERROR memory exceeded", []byte{0x04, 0x05, 0x01, 0x16}, false, 5, 22, false},
		{"RP-ERROR with diagnostic and user data", []byte{0x04, 0x09, 0x02, 0x6f, 0x01, 0x41, 0x01, 0x00}, false, 9, 111, false},
		{"RP-ERROR without its cause", []byte{0x04, 0x05, 0x01}, false, 5, 0, true},
		{"RP-ERROR cause length 0", []byte{0x04, 0x05, 0x00, 0x16}, false, 5, 0, true},
		{"RP-ERROR cause length past the end", []byte{0x04, 0x05, 0x03, 0x16}, false, 5, 0, true},
		{"network RP-ACK is not a phone report", []byte{0x03, 0x05}, false, 5, 0, true},
		{"too short", []byte{0x02}, false, 0, 0, true},
	} {
		ok, ref, cause, err := DecodeRPReport(c.in)
		if (err != nil) != c.err {
			t.Errorf("%s: err %v", c.name, err)
			continue
		}
		if !c.err && (ok != c.ok || ref != c.ref || cause != c.wcau) {
			t.Errorf("%s: ok %v ref %d cause %d, want %v %d %d", c.name, ok, ref, cause, c.ok, c.ref, c.wcau)
		}
	}
}

func TestEncodeDeliverWith(t *testing.T) {
	// EncodeDeliver is EncodeDeliverWith with the proven SMSC and reference
	if !bytes.Equal(EncodeDeliver("1001001489", "Hello", frozen), EncodeDeliverWith("1001001489", "Hello", frozen, "7", 0)) {
		t.Fatal("EncodeDeliver differs from EncodeDeliverWith(..., \"7\", 0)")
	}
	rp := EncodeDeliverWith("1001001489", "Hello", frozen, "+91990000123", 0x2a)
	want := []byte{0x01, 0x2a, 0x07, 0x91, 0x19, 0x99, 0x00, 0x00, 0x21, 0xf3, 0x00}
	if !bytes.HasPrefix(rp, want) {
		t.Fatalf("RP prefix = % x, want % x", rp[:len(want)], want)
	}
	// the TPDU after it is the one EncodeDeliver builds
	ref := EncodeDeliver("1001001489", "Hello", frozen)
	if !bytes.Equal(rp[len(want):], ref[6:]) {
		t.Fatalf("TPDU = % x, want % x", rp[len(want):], ref[6:])
	}
	if got := EncodeDeliverWith("1", "x", frozen, "", 1); got[2] != 0x00 || got[3] != 0x00 {
		t.Fatalf("no SMSC: RP-OA/RP-DA = % x, want 00 00", got[2:4])
	}
}

// Malformed input must be rejected, never panic: these bytes come off the network.
func TestDecodeMORejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"empty":           {},
		"rp header only":  {0x00, 0x01},
		"truncated rp-oa": {0x00, 0x01, 0x09},
		"no user data":    {0x00, 0x01, 0x00, 0x00},
		"zero-length ud":  {0x00, 0x01, 0x00, 0x00, 0x00},
		"tpdu truncated":  {0x00, 0x01, 0x00, 0x00, 0x04, 0x01, 0x00, 0x0a, 0x91},
		"da overruns":     {0x00, 0x01, 0x00, 0x00, 0x05, 0x01, 0x00, 0x7f, 0x91, 0x11},
		"deliver not submit": append([]byte{0x00, 0x01, 0x00, 0x00, 0x0a},
			0x04, 0x0a, 0x91, 0x11, 0x22, 0x33, 0x44, 0x55, 0x00, 0x00),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked on malformed input: %v", r)
				}
			}()
			if mo, err := DecodeMO(in); err == nil {
				t.Fatalf("accepted malformed input, got %+v", mo)
			}
		})
	}
}

// A submit carrying a user-data header must skip it and still decode the text.
func TestDecodeMOWithUserDataHeader(t *testing.T) {
	udh := []byte{0x05, 0x00, 0x03, 0x2a, 0x02, 0x01} // 6-byte concat header
	ud, udl := packAfterHeader(udh, "hdr test")
	tp := []byte{0x41, 0x00, 0x0a, 0x91} // MTI=SUBMIT + UDHI
	tp = append(tp, EncodeBCDAddr("1001001488")...)
	tp = append(tp, 0x00, 0x00, byte(udl))
	tp = append(tp, ud...)
	rp := append([]byte{0x00, 0x09, 0x00, 0x02, 0x91, 0xf7, byte(len(tp))}, tp...)

	mo, err := DecodeMO(rp)
	if err != nil {
		t.Fatalf("decode with UDH: %v", err)
	}
	if mo.Recipient != "1001001488" {
		t.Errorf("recipient = %q", mo.Recipient)
	}
	if mo.Text != "hdr test" {
		t.Errorf("text = %q, want %q (the fill bits after the header)", mo.Text, "hdr test")
	}
}

// packAfterHeader is the GSM 7-bit user data of a message with a header: the header octets, the fill bits that put
// the first character on a septet boundary, then the text (TS 23.040 9.2.3.24). It returns the octets and the UDL
// in septets, header included.
func packAfterHeader(udh []byte, text string) ([]byte, int) {
	hdrSeptets := (len(udh)*8 + 6) / 7
	var bits []byte // one entry per bit, least significant first
	for _, b := range udh {
		for k := 0; k < 8; k++ {
			bits = append(bits, (b>>k)&1)
		}
	}
	for len(bits) < hdrSeptets*7 {
		bits = append(bits, 0)
	}
	packed, n := GSM7Pack(text)
	for k := 0; k < n*7; k++ {
		bits = append(bits, (packed[k/8]>>(k%8))&1)
	}
	out := make([]byte, (len(bits)+7)/8)
	for k, v := range bits {
		out[k/8] |= v << (k % 8)
	}
	return out, hdrSeptets + n
}
