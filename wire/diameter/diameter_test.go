// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package diameter

import (
	"bytes"
	"net"
	"testing"
)

// AVP encoding must be 4-byte aligned with correct declared length and padding.
func TestAVPEncodePaddingAndLength(t *testing.T) {
	// Framed-IP-Address style OctetString of 3 bytes -> length 11, padded to 12.
	a := Octet(1, 0, true, []byte{0xAA, 0xBB, 0xCC})
	enc := a.Encode()
	if len(enc)%4 != 0 {
		t.Fatalf("encoded AVP not 4-byte aligned: %d", len(enc))
	}
	declared := int(enc[5])<<16 | int(enc[6])<<8 | int(enc[7])
	if declared != 11 {
		t.Fatalf("declared length = %d, want 11", declared)
	}
	if len(enc) != 12 {
		t.Fatalf("padded length = %d, want 12", len(enc))
	}
	if enc[4]&FlagMandatory == 0 {
		t.Fatalf("mandatory flag not set")
	}
}

// Vendor-specific AVPs carry the 4-byte vendor id and set the V flag.
func TestVendorAVPRoundTrip(t *testing.T) {
	a := U32(516, Vendor3GPP, true, 128000) // Max-Requested-Bandwidth-UL
	enc := a.Encode()
	got, err := DecodeAVPs(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("decoded %d AVPs, want 1", len(got))
	}
	if got[0].VendorID != Vendor3GPP {
		t.Fatalf("vendor = %d, want %d", got[0].VendorID, Vendor3GPP)
	}
	v, err := got[0].U32Val()
	if err != nil || v != 128000 {
		t.Fatalf("value = %d (%v), want 128000", v, err)
	}
}

// Grouped AVPs must round-trip: encode children, decode back, recover values.
func TestGroupedAVPRoundTrip(t *testing.T) {
	inner := Grouped(519, Vendor3GPP, true, // Media-Sub-Component
		U32(226, Vendor3GPP, true, 1),                                         // Flow-Number
		Str(507, Vendor3GPP, true, "permit out 17 from any to 10.0.0.5 5000"), // Flow-Description
		I32(511, Vendor3GPP, true, 2),                                         // Flow-Status = ENABLED(2)
	)
	enc := inner.Encode()
	decoded, err := DecodeAVPs(enc)
	if err != nil {
		t.Fatal(err)
	}
	kids, err := decoded[0].SubAVPs()
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 3 {
		t.Fatalf("grouped children = %d, want 3", len(kids))
	}
	fd, err := Find(kids, 507, Vendor3GPP)
	if err != nil {
		t.Fatal(err)
	}
	if fd.StrVal() != "permit out 17 from any to 10.0.0.5 5000" {
		t.Fatalf("flow-description = %q", fd.StrVal())
	}
	fs, _ := Find(kids, 511, Vendor3GPP)
	if v, _ := fs.U32Val(); v != 2 {
		t.Fatalf("flow-status = %d, want 2", v)
	}
}

// A full request must round-trip through Encode/DecodeMessage with ids intact.
func TestMessageRoundTrip(t *testing.T) {
	req := Message{
		Flags:    CmdRequest | CmdProxiable,
		Code:     CmdAA,
		AppID:    AppRx,
		HopByHop: 0xdeadbeef,
		EndToEnd: 0x01020304,
		AVPs: []AVP{
			Str(263, 0, true, "sess;1;2"),                // Session-Id
			Addr(8, 0, true, net.ParseIP("10.20.30.40")), // Framed-IP-Address
			U32(520, Vendor3GPP, true, 0),                // Media-Type AUDIO
		},
	}
	wire := req.Encode()
	if len(wire)%4 != 0 {
		t.Fatalf("message not 4-byte aligned: %d", len(wire))
	}
	got, err := DecodeMessage(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsRequest() || got.Code != CmdAA || got.AppID != AppRx {
		t.Fatalf("header mismatch: req=%v code=%d app=%d", got.IsRequest(), got.Code, got.AppID)
	}
	if got.HopByHop != 0xdeadbeef || got.EndToEnd != 0x01020304 {
		t.Fatalf("ids mismatch h=%x e=%x", got.HopByHop, got.EndToEnd)
	}
	ip, err := mustFind(t, got.AVPs, 8, 0).IPVal()
	if err != nil {
		t.Fatal(err)
	}
	if !ip.Equal(net.ParseIP("10.20.30.40")) {
		t.Fatalf("framed-ip = %v", ip)
	}
	// Re-encode must be byte-identical (canonical form).
	if !bytes.Equal(wire, got.Encode()) {
		t.Fatalf("re-encode not identical")
	}
}

// Answer() clears R and preserves correlation ids.
func TestAnswerCorrelation(t *testing.T) {
	req := Message{Flags: CmdRequest, Code: CmdSAR, AppID: AppCx, HopByHop: 7, EndToEnd: 9}
	ans := req.Answer()
	if ans.IsRequest() {
		t.Fatal("answer still has R bit")
	}
	if ans.Code != CmdSAR || ans.AppID != AppCx || ans.HopByHop != 7 || ans.EndToEnd != 9 {
		t.Fatalf("answer correlation wrong: %+v", ans)
	}
}

func mustFind(t *testing.T, avps []AVP, code, vendor uint32) AVP {
	t.Helper()
	a, err := Find(avps, code, vendor)
	if err != nil {
		t.Fatalf("AVP %d/%d not found", code, vendor)
	}
	return a
}
