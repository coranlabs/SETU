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
	packed, septets := GSM7Pack("hdr test")
	udh := []byte{0x05, 0x00, 0x03, 0x2a, 0x02, 0x01} // 6-byte concat header
	tp := []byte{0x41, 0x00, 0x0a, 0x91}              // MTI=SUBMIT + UDHI
	tp = append(tp, EncodeBCDAddr("1001001488")...)
	tp = append(tp, 0x00, 0x00, byte(septets+7))
	tp = append(tp, udh...)
	tp = append(tp, packed...)
	rp := append([]byte{0x00, 0x09, 0x00, 0x02, 0x91, 0xf7, byte(len(tp))}, tp...)

	mo, err := DecodeMO(rp)
	if err != nil {
		t.Fatalf("decode with UDH: %v", err)
	}
	if mo.Recipient != "1001001488" {
		t.Errorf("recipient = %q", mo.Recipient)
	}
}
