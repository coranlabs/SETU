// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package cx

import (
	"bytes"
	"context"
	"testing"

	api "github.com/coranlabs/SETU/api/v1"
	"github.com/coranlabs/SETU/ims/ident"
	"github.com/coranlabs/SETU/wire/diameter"
)

// fakeCore scripts Subscriber/Auth so the MAA/SAA wire layouts can be
// characterized without a UDR.
type fakeCore struct{ vec api.AuthVector }

func (f *fakeCore) Name() string                      { return "fake" }
func (f *fakeCore) Capabilities() api.Caps            { return api.Caps{} }
func (f *fakeCore) Policy() api.PolicyBackend         { return nil }
func (f *fakeCore) Events() api.EventSource           { return nil }
func (f *fakeCore) Subscriber() api.SubscriberBackend { return f }
func (f *fakeCore) Auth() api.AuthBackend             { return f }

func (f *fakeCore) Exists(context.Context, string) (bool, error)   { return true, nil }
func (f *fakeCore) MSISDN(context.Context, string) (string, error) { return "1001001488", nil }
func (f *fakeCore) Vectors(context.Context, api.AuthChallenge) ([]api.AuthVector, error) {
	return []api.AuthVector{f.vec}, nil
}

func pat(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

// TestMARLayout pins the MAA wire layout byte-for-byte: this is what the S-CSCF
// builds its 401 AKAv1-MD5 challenge from (proven live), so it must never drift.
func TestMARLayout(t *testing.T) {
	vec := api.AuthVector{RAND: pat(0x11, 16), AUTN: pat(0x22, 16), XRES: pat(0x33, 8), CK: pat(0x44, 16), IK: pat(0x55, 16)}
	a := &Adapter{
		OriginHost:  "hss.ims.mnc001.mcc001.3gppnetwork.org",
		OriginRealm: "ims.mnc001.mcc001.3gppnetwork.org",
		SCSCFName:   "sip:192.0.2.10:6060",
		PLMN:        ident.PLMN{MCC: "001", MNC: "01"},
		Core:        &fakeCore{vec: vec},
	}
	mar := diameter.Message{
		Flags: diameter.CmdRequest, Code: diameter.CmdMAR, AppID: diameter.AppCx,
		AVPs: []diameter.AVP{
			diameter.Str(diameter.AVPSessionID, 0, true, "scscf;1;1"),
			diameter.Str(diameter.AVPUserName, 0, true, "001010000000001@ims.mnc001.mcc001.3gppnetwork.org"),
		},
	}
	ans, err := a.Handle(nil, mar)
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := mustFind(t, ans.AVPs, 268, 0).U32Val()
	if rc != 2001 {
		t.Fatalf("MAA Result-Code = %d, want 2001", rc)
	}
	item := mustFind(t, ans.AVPs, diameter.AVPSIPAuthDataItem, diameter.Vendor3GPP)
	kids, err := item.SubAVPs()
	if err != nil {
		t.Fatal(err)
	}
	if s := mustFind(t, kids, diameter.AVPSIPAuthScheme, diameter.Vendor3GPP).StrVal(); s != "Digest-AKAv1-MD5" {
		t.Fatalf("scheme = %q", s)
	}
	auth := mustFind(t, kids, diameter.AVPSIPAuthenticate, diameter.Vendor3GPP).Data
	if !bytes.Equal(auth, append(pat(0x11, 16), pat(0x22, 16)...)) {
		t.Fatalf("SIP-Authenticate = % x, want RAND(16)||AUTN(16)", auth)
	}
	if x := mustFind(t, kids, diameter.AVPSIPAuthorization, diameter.Vendor3GPP).Data; !bytes.Equal(x, pat(0x33, 8)) {
		t.Fatalf("SIP-Authorization (XRES) = % x", x)
	}
	if ck := mustFind(t, kids, diameter.AVPConfidentialityKey, diameter.Vendor3GPP).Data; !bytes.Equal(ck, pat(0x44, 16)) {
		t.Fatalf("CK = % x", ck)
	}
	if ik := mustFind(t, kids, diameter.AVPIntegrityKey, diameter.Vendor3GPP).Data; !bytes.Equal(ik, pat(0x55, 16)) {
		t.Fatalf("IK = % x", ik)
	}
	if impi := mustFind(t, ans.AVPs, diameter.AVPUserName, 0).StrVal(); impi != "001010000000001@ims.mnc001.mcc001.3gppnetwork.org" {
		t.Fatalf("User-Name = %q", impi)
	}
}

// TestUARAndSAR pins the UAA Server-Name and the SAA carrying User-Data XML.
func TestUARAndSAR(t *testing.T) {
	a := &Adapter{
		OriginHost:  "hss.ims.mnc001.mcc001.3gppnetwork.org",
		OriginRealm: "ims.mnc001.mcc001.3gppnetwork.org",
		SCSCFName:   "sip:192.0.2.10:6060",
		PLMN:        ident.PLMN{MCC: "001", MNC: "01"},
		Core:        &fakeCore{},
	}
	uar := diameter.Message{Flags: diameter.CmdRequest, Code: diameter.CmdUAR, AppID: diameter.AppCx,
		AVPs: []diameter.AVP{diameter.Str(diameter.AVPUserName, 0, true, "001010000000001@x")}}
	ans, err := a.Handle(nil, uar)
	if err != nil {
		t.Fatal(err)
	}
	if sn := mustFind(t, ans.AVPs, diameter.AVPServerName, diameter.Vendor3GPP).StrVal(); sn != "sip:192.0.2.10:6060" {
		t.Fatalf("UAA Server-Name = %q", sn)
	}

	sar := diameter.Message{Flags: diameter.CmdRequest, Code: diameter.CmdSAR, AppID: diameter.AppCx,
		AVPs: []diameter.AVP{diameter.Str(diameter.AVPUserName, 0, true, "001010000000001@x")}}
	ans, err = a.Handle(nil, sar)
	if err != nil {
		t.Fatal(err)
	}
	xml := mustFind(t, ans.AVPs, diameter.AVPUserData, diameter.Vendor3GPP).Data
	for _, want := range []string{"<IMSSubscription", "1001001488", "001010000000001"} {
		if !bytes.Contains(xml, []byte(want)) {
			t.Fatalf("SAA User-Data missing %q in:\n%s", want, xml)
		}
	}
}

func mustFind(t *testing.T, avps []diameter.AVP, code, vendor uint32) diameter.AVP {
	t.Helper()
	a, err := diameter.Find(avps, code, vendor)
	if err != nil {
		t.Fatalf("AVP %d (vendor %d) not found", code, vendor)
	}
	return a
}
