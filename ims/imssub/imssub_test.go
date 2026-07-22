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
