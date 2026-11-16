// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package imssub

import (
	"strings"
	"testing"

	"github.com/coranlabs/SETU/ims/ident"
)

func plmn() ident.PLMN { return ident.PLMN{MCC: "001", MNC: "01"} }

// A plain VoNR profile must be well-formed, carry the IMPI as PrivateID, the three
// public identities with correct barring, and validate.
func TestBuildPlainVoNRProfile(t *testing.T) {
	xmlBytes, err := Build(Subscriber{IMSI: "001010000000001", MSISDN: "9000000001", PLMN: plmn()})
	if err != nil {
		t.Fatal(err)
	}
	s := string(xmlBytes)
	if !strings.HasPrefix(s, `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Fatalf("missing XML declaration: %q", s[:40])
	}

	// well-formed + round-trips
	doc, err := Parse(xmlBytes)
	if err != nil {
		t.Fatalf("not well-formed: %v", err)
	}
	if err := Validate(doc); err != nil {
		t.Fatalf("validate: %v", err)
	}

	wantPriv := "001010000000001@ims.mnc001.mcc001.3gppnetwork.org"
	if doc.PrivateID != wantPriv {
		t.Fatalf("PrivateID = %q, want %q", doc.PrivateID, wantPriv)
	}
	if len(doc.ServiceProfiles) != 1 {
		t.Fatalf("want 1 ServiceProfile, got %d", len(doc.ServiceProfiles))
	}
	ids := doc.ServiceProfiles[0].PublicIdentities
	if len(ids) != 3 {
		t.Fatalf("want 3 PublicIdentity, got %d", len(ids))
	}
	// temp IMPU (IMSI) barred; MSISDN identities not barred
	assertIdentity(t, ids[0], "sip:001010000000001@ims.mnc001.mcc001.3gppnetwork.org", 1)
	assertIdentity(t, ids[1], "sip:9000000001@ims.mnc001.mcc001.3gppnetwork.org", 0)
	assertIdentity(t, ids[2], "tel:+9000000001", 0)

	// no iFC in a plain VoNR profile
	if len(doc.ServiceProfiles[0].InitialFilterCriteria) != 0 {
		t.Fatalf("plain profile should have no iFC")
	}
}

// With an Application Server trigger, a valid iFC must be emitted and parse back.
func TestBuildWithApplicationServer(t *testing.T) {
	xmlBytes, err := Build(Subscriber{
		IMSI: "001010000000002", MSISDN: "9000000002", PLMN: plmn(),
		AppServers: []AppServerTrigger{
			{ServerName: "sip:mmtel-as.ims.mnc001.mcc001.3gppnetwork.org:5060", Method: "INVITE", DefaultHandling: 0, Priority: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(xmlBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(doc); err != nil {
		t.Fatal(err)
	}
	ifcs := doc.ServiceProfiles[0].InitialFilterCriteria
	if len(ifcs) != 1 {
		t.Fatalf("want 1 iFC, got %d", len(ifcs))
	}
	if ifcs[0].ApplicationServer.ServerName != "sip:mmtel-as.ims.mnc001.mcc001.3gppnetwork.org:5060" {
		t.Fatalf("AS server name = %q", ifcs[0].ApplicationServer.ServerName)
	}
	if ifcs[0].TriggerPoint == nil || len(ifcs[0].TriggerPoint.SPTs) != 1 || ifcs[0].TriggerPoint.SPTs[0].Method != "INVITE" {
		t.Fatalf("trigger point/method wrong: %+v", ifcs[0].TriggerPoint)
	}
}

// Validate must reject malformed profiles (missing PrivateID / empty Identity).
func TestValidateRejectsMalformed(t *testing.T) {
	if err := Validate(IMSSubscription{ServiceProfiles: []ServiceProfile{{PublicIdentities: []PublicIdentity{{Identity: "sip:x"}}}}}); err == nil {
		t.Fatal("expected error on missing PrivateID")
	}
	if err := Validate(IMSSubscription{PrivateID: "x", ServiceProfiles: []ServiceProfile{{PublicIdentities: []PublicIdentity{{Identity: ""}}}}}); err == nil {
		t.Fatal("expected error on empty Identity")
	}
	if err := Validate(IMSSubscription{PrivateID: "x", ServiceProfiles: nil}); err == nil {
		t.Fatal("expected error on no ServiceProfile")
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	if _, err := Build(Subscriber{IMSI: "abc", MSISDN: "9000000001", PLMN: plmn()}); err == nil {
		t.Fatal("expected error on non-digit IMSI")
	}
}

func assertIdentity(t *testing.T, p PublicIdentity, wantID string, wantBarring int) {
	t.Helper()
	if p.Identity != wantID {
		t.Fatalf("Identity = %q, want %q", p.Identity, wantID)
	}
	if p.BarringIndication == nil || *p.BarringIndication != wantBarring {
		t.Fatalf("BarringIndication for %s = %v, want %d", wantID, p.BarringIndication, wantBarring)
	}
}

// The two TossiCore MCX rules (the third-party REGISTER, and requests for the MC services' PSIs) must come out as two
// iFCs whose conditions are all ANDed (group 0, ConditionTypeCNF 0), each condition in its own SPT box.
func TestBuildMCXTriggers(t *testing.T) {
	orig := 0 // SessionCase 0: originating (TS 29.228)
	const mcx = "sip:192.0.2.20:5070"
	const psi = "^sip:(mcptt|mcdata|mcvideo)@"

	// 1. a subscriber with the two MCX rules
	xmlBytes, err := Build(Subscriber{
		IMSI: "001010100007487", MSISDN: "9000007487", PLMN: plmn(),
		AppServers: []AppServerTrigger{
			{ServerName: mcx, Method: "REGISTER", SessionCase: &orig, Priority: 10, DefaultHandling: 0},
			{ServerName: mcx, RequestURI: psi, SessionCase: &orig, Priority: 20, DefaultHandling: 1},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// 2. read it back, as the S-CSCF would, and check its shape
	doc, err := Parse(xmlBytes)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Validate(doc); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// 3. two iFCs
	ifcs := doc.ServiceProfiles[0].InitialFilterCriteria
	if len(ifcs) != 2 {
		t.Fatalf("want 2 iFCs, got %d", len(ifcs))
	}

	// 4. rule 1: REGISTER and originating; the sign-in goes on if MCX does not answer (DefaultHandling 0)
	reg := ifcs[0]
	if reg.Priority != 10 || reg.ApplicationServer.ServerName != mcx || reg.ApplicationServer.DefaultHandling != 0 {
		t.Fatalf("rule 1: %+v", reg)
	}
	if reg.TriggerPoint == nil || reg.TriggerPoint.ConditionTypeCNF != 0 || len(reg.TriggerPoint.SPTs) != 2 {
		t.Fatalf("rule 1 trigger point: %+v", reg.TriggerPoint)
	}
	if s := reg.TriggerPoint.SPTs; s[0].Method != "REGISTER" || s[1].SessionCase == nil || *s[1].SessionCase != 0 {
		t.Fatalf("rule 1 conditions: %+v", s)
	}

	// 5. rule 2: the MC PSIs and originating; a request fails at once if MCX does not answer (DefaultHandling 1).
	// The address must be in the RequestURI box, never in Method (that compiles, and nothing would ever match)
	mc := ifcs[1]
	if mc.Priority != 20 || mc.ApplicationServer.ServerName != mcx || mc.ApplicationServer.DefaultHandling != 1 {
		t.Fatalf("rule 2: %+v", mc)
	}
	if mc.TriggerPoint == nil || mc.TriggerPoint.ConditionTypeCNF != 0 || len(mc.TriggerPoint.SPTs) != 2 {
		t.Fatalf("rule 2 trigger point: %+v", mc.TriggerPoint)
	}
	if s := mc.TriggerPoint.SPTs; s[0].RequestURI != psi || s[0].Method != "" || s[1].SessionCase == nil || *s[1].SessionCase != 0 {
		t.Fatalf("rule 2 conditions: %+v", s)
	}

	// 6. every condition in group 0: with ConditionTypeCNF 0 that makes them all AND
	for i, ifc := range ifcs {
		for j, spt := range ifc.TriggerPoint.SPTs {
			if spt.Group != 0 {
				t.Errorf("rule %d condition %d is in group %d, want 0", i+1, j+1, spt.Group)
			}
		}
	}
}

// The S-CSCF checks the profile against CxDataType_Rel7.xsd, where the order of ServiceProfile's children is fixed:
// PublicIdentity, CoreNetworkServicesAuthorization, InitialFilterCriteria. Parse (above) does not mind the order, so
// it is checked here on the text itself (11-10: the iFCs came first and every registration with them failed).
func TestServiceProfileChildrenAreInSchemaOrder(t *testing.T) {
	orig := 0
	xmlBytes, err := Build(Subscriber{
		IMSI: "001010100007487", MSISDN: "9000007487", PLMN: plmn(),
		AppServers: []AppServerTrigger{{ServerName: "sip:192.0.2.20:5070", Method: "REGISTER", SessionCase: &orig, Priority: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(xmlBytes)
	last := strings.LastIndex(s, "</PublicIdentity>")
	cn := strings.Index(s, "<CoreNetworkServicesAuthorization>")
	ifc := strings.Index(s, "<InitialFilterCriteria>")
	if last < 0 || cn < 0 || ifc < 0 {
		t.Fatalf("missing elements (PublicIdentity end %d, CoreNetworkServicesAuthorization %d, InitialFilterCriteria %d):\n%s", last, cn, ifc, s)
	}
	if !(last < cn && cn < ifc) {
		t.Fatalf("order PublicIdentity(%d) < CoreNetworkServicesAuthorization(%d) < InitialFilterCriteria(%d) broken:\n%s", last, cn, ifc, s)
	}
}
