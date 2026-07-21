// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package ident

import "testing"

// PLMN 001/01 must yield the exact home domain the deployed IMS uses (TS 23.003).
func TestHomeDomainMatchesDeployment(t *testing.T) {
	p := PLMN{MCC: "001", MNC: "01"}
	if got := p.HomeDomain(); got != "ims.mnc001.mcc001.3gppnetwork.org" {
		t.Fatalf("home domain = %q, want ims.mnc001.mcc001.3gppnetwork.org", got)
	}
	// 3-digit MNC stays 3 digits.
	if got := (PLMN{MCC: "310", MNC: "260"}).HomeDomain(); got != "ims.mnc260.mcc310.3gppnetwork.org" {
		t.Fatalf("3-digit MNC domain = %q", got)
	}
}

func TestFromIMSI(t *testing.T) {
	p, err := FromIMSI("001010000000001", 2)
	if err != nil {
		t.Fatal(err)
	}
	if p.MCC != "001" || p.MNC != "01" {
		t.Fatalf("plmn = %+v, want {001 01}", p)
	}
	if _, err := FromIMSI("00101ABC", 2); err == nil {
		t.Fatal("expected error on non-digit IMSI")
	}
}

func TestIMPIAndTempIMPU(t *testing.T) {
	p := PLMN{MCC: "001", MNC: "01"}
	imsi := "001010000000001"
	impi, err := p.IMPI(imsi)
	if err != nil {
		t.Fatal(err)
	}
	if impi != "001010000000001@ims.mnc001.mcc001.3gppnetwork.org" {
		t.Fatalf("IMPI = %q", impi)
	}
	impu, _ := p.TempIMPU(imsi)
	if impu != "sip:001010000000001@ims.mnc001.mcc001.3gppnetwork.org" {
		t.Fatalf("temp IMPU = %q", impu)
	}
}

// The SIP IMPU must match the P-Associated-URI form the deployed S-CSCF emits.
func TestMSISDNIdentities(t *testing.T) {
	p := PLMN{MCC: "001", MNC: "01"}
	sip, tel, err := p.MSISDNIdentities("9000000001")
	if err != nil {
		t.Fatal(err)
	}
	if sip != "sip:9000000001@ims.mnc001.mcc001.3gppnetwork.org" {
		t.Fatalf("SIP IMPU = %q", sip)
	}
	if tel != "tel:+9000000001" {
		t.Fatalf("tel URI = %q", tel)
	}
	// leading + is tolerated on input
	sip2, _, _ := p.MSISDNIdentities("+9000000001")
	if sip2 != sip {
		t.Fatalf("+ prefix changed result: %q vs %q", sip2, sip)
	}
}

func TestGPSIRoundTrip(t *testing.T) {
	g, err := GPSI("9000000001")
	if err != nil {
		t.Fatal(err)
	}
	if g != "msisdn-9000000001" {
		t.Fatalf("GPSI = %q", g)
	}
	// idempotent on already-prefixed input
	g2, _ := GPSI("msisdn-9000000001")
	if g2 != g {
		t.Fatalf("GPSI not idempotent: %q", g2)
	}
	m, err := MSISDNFromGPSI(g)
	if err != nil || m != "9000000001" {
		t.Fatalf("MSISDNFromGPSI = %q (%v)", m, err)
	}
	if _, err := MSISDNFromGPSI("9000000001"); err == nil {
		t.Fatal("expected error on missing prefix")
	}
}
