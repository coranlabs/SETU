// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package imssub builds the 3GPP TS 29.228 IMSSubscription (Cx User-Data) XML that
// the Cx-SAR shim returns in an SAA, so a Kamailio S-CSCF can store the user profile
// and iFC. For basic VoNR with no Application Servers the profile is a single
// ServiceProfile carrying the subscriber's public identities.
package imssub

import (
	"encoding/xml"
	"fmt"

	"github.com/coranlabs/SETU/ims/ident"
)

// IMSSubscription is the root of the Cx User-Data document (TS 29.228 §6.6).
type IMSSubscription struct {
	XMLName         xml.Name         `xml:"IMSSubscription"`
	PrivateID       string           `xml:"PrivateID"`
	ServiceProfiles []ServiceProfile `xml:"ServiceProfile"`
}

// ServiceProfile groups public identities, iFC and CN service authorization.
type ServiceProfile struct {
	PublicIdentities                 []PublicIdentity        `xml:"PublicIdentity"`
	InitialFilterCriteria            []InitialFilterCriteria `xml:"InitialFilterCriteria,omitempty"`
	CoreNetworkServicesAuthorization *CNServicesAuth         `xml:"CoreNetworkServicesAuthorization,omitempty"`
}

// PublicIdentity is one IMPU. BarringIndication=1 marks it usable for registration
// only (the IMSI-derived temporary IMPU); MSISDN identities are non-barred.
type PublicIdentity struct {
	BarringIndication *int   `xml:"BarringIndication,omitempty"`
	Identity          string `xml:"Identity"`
}

// InitialFilterCriteria triggers routing to an Application Server (optional).
type InitialFilterCriteria struct {
	Priority          int               `xml:"Priority"`
	TriggerPoint      *TriggerPoint     `xml:"TriggerPoint,omitempty"`
	ApplicationServer ApplicationServer `xml:"ApplicationServer"`
}

// TriggerPoint / SPT describe when the iFC fires (TS 29.228 §6.6).
type TriggerPoint struct {
	ConditionTypeCNF int   `xml:"ConditionTypeCNF"`
	SPTs             []SPT `xml:"SPT"`
}

// SPT is a Service Point Trigger.
type SPT struct {
	ConditionNegated int    `xml:"ConditionNegated"`
	Group            int    `xml:"Group"`
	Method           string `xml:"Method,omitempty"`
	SIPHeader        string `xml:"SIPHeader>Header,omitempty"`
}

// ApplicationServer names the AS and its default handling.
type ApplicationServer struct {
	ServerName      string `xml:"ServerName"`
	DefaultHandling int    `xml:"DefaultHandling"` // 0 = SESSION_CONTINUED, 1 = SESSION_TERMINATED
}

// CNServicesAuth carries the subscribed media profile.
type CNServicesAuth struct {
	SubscribedMediaProfileId *int `xml:"SubscribedMediaProfileId,omitempty"`
}

// AppServerTrigger describes an optional AS/iFC to include.
type AppServerTrigger struct {
	ServerName      string // sip: URI of the AS
	Method          string // e.g. "INVITE"; empty => no trigger point (unconditional)
	DefaultHandling int    // 0 continued, 1 terminated
	Priority        int
}

// Subscriber describes who the profile is for.
type Subscriber struct {
	IMSI   string
	MSISDN string
	PLMN   ident.PLMN
	// Optional Application Server triggers; empty => plain VoNR profile (no AS).
	AppServers []AppServerTrigger
}

func intp(v int) *int { return &v }

// Build assembles and marshals the IMSSubscription XML (with declaration).
func Build(s Subscriber) ([]byte, error) {
	impi, err := s.PLMN.IMPI(s.IMSI)
	if err != nil {
		return nil, fmt.Errorf("imssub: %w", err)
	}
	tempIMPU, err := s.PLMN.TempIMPU(s.IMSI)
	if err != nil {
		return nil, fmt.Errorf("imssub: %w", err)
	}
	sipIMPU, telIMPU, err := s.PLMN.MSISDNIdentities(s.MSISDN)
	if err != nil {
		return nil, fmt.Errorf("imssub: %w", err)
	}

	sp := ServiceProfile{
		PublicIdentities: []PublicIdentity{
			{BarringIndication: intp(1), Identity: tempIMPU}, // registration-only
			{BarringIndication: intp(0), Identity: sipIMPU},
			{BarringIndication: intp(0), Identity: telIMPU},
		},
		CoreNetworkServicesAuthorization: &CNServicesAuth{SubscribedMediaProfileId: intp(0)},
	}
	for _, as := range s.AppServers {
		ifc := InitialFilterCriteria{
			Priority:          as.Priority,
			ApplicationServer: ApplicationServer{ServerName: as.ServerName, DefaultHandling: as.DefaultHandling},
		}
		if as.Method != "" {
			ifc.TriggerPoint = &TriggerPoint{
				ConditionTypeCNF: 0,
				SPTs:             []SPT{{ConditionNegated: 0, Group: 0, Method: as.Method}},
			}
		}
		sp.InitialFilterCriteria = append(sp.InitialFilterCriteria, ifc)
	}

	doc := IMSSubscription{PrivateID: impi, ServiceProfiles: []ServiceProfile{sp}}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("imssub: marshal: %w", err)
	}
	return append([]byte(xml.Header), body...), nil
}

// Parse unmarshals an IMSSubscription document (used for validation/round-trip).
func Parse(b []byte) (IMSSubscription, error) {
	var doc IMSSubscription
	if err := xml.Unmarshal(b, &doc); err != nil {
		return IMSSubscription{}, fmt.Errorf("imssub: parse: %w", err)
	}
	return doc, nil
}

// Validate checks the structural rules a Kamailio S-CSCF (ims_registrar_scscf
// userdata_parser) requires: a PrivateID and at least one ServiceProfile with at
// least one PublicIdentity carrying a non-empty Identity.
func Validate(doc IMSSubscription) error {
	if doc.PrivateID == "" {
		return fmt.Errorf("imssub: missing PrivateID")
	}
	if len(doc.ServiceProfiles) == 0 {
		return fmt.Errorf("imssub: no ServiceProfile")
	}
	for i, sp := range doc.ServiceProfiles {
		if len(sp.PublicIdentities) == 0 {
			return fmt.Errorf("imssub: ServiceProfile[%d] has no PublicIdentity", i)
		}
		for j, pid := range sp.PublicIdentities {
			if pid.Identity == "" {
				return fmt.Errorf("imssub: ServiceProfile[%d].PublicIdentity[%d] empty Identity", i, j)
			}
		}
		for k, ifc := range sp.InitialFilterCriteria {
			if ifc.ApplicationServer.ServerName == "" {
				return fmt.Errorf("imssub: ServiceProfile[%d].iFC[%d] missing ApplicationServer/ServerName", i, k)
			}
		}
	}
	return nil
}
