// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package ident derives 3GPP IMS identities (private/public) from IMSI and MSISDN
// per 3GPP TS 23.003, matching the home-domain convention the deployed IMS uses
// (e.g. ims.mnc001.mcc001.3gppnetwork.org for PLMN 001/01).
package ident

import (
	"fmt"
	"strings"
)

// PLMN identifies the home network.
type PLMN struct {
	MCC string // 3 digits
	MNC string // 2 or 3 digits
}

// HomeDomain returns the IMS home domain, with MNC zero-padded to 3 digits
// (TS 23.003 §13.2): ims.mnc<MNC>.mcc<MCC>.3gppnetwork.org.
func (p PLMN) HomeDomain() string {
	mnc := p.MNC
	if len(mnc) == 2 {
		mnc = "0" + mnc
	}
	return fmt.Sprintf("ims.mnc%s.mcc%s.3gppnetwork.org", mnc, p.MCC)
}

// FromIMSI extracts the PLMN from an IMSI given the MNC length (2 or 3).
func FromIMSI(imsi string, mncLen int) (PLMN, error) {
	if err := validateDigits(imsi); err != nil {
		return PLMN{}, fmt.Errorf("imsi: %w", err)
	}
	if mncLen != 2 && mncLen != 3 {
		return PLMN{}, fmt.Errorf("mncLen must be 2 or 3, got %d", mncLen)
	}
	if len(imsi) < 3+mncLen {
		return PLMN{}, fmt.Errorf("imsi %q too short for mncLen %d", imsi, mncLen)
	}
	return PLMN{MCC: imsi[0:3], MNC: imsi[3 : 3+mncLen]}, nil
}

// IMPI returns the IMS Private Identity derived from the IMSI (TS 23.003 §13.3):
// <IMSI>@<home-domain>.
func (p PLMN) IMPI(imsi string) (string, error) {
	if err := validateDigits(imsi); err != nil {
		return "", fmt.Errorf("imsi: %w", err)
	}
	return imsi + "@" + p.HomeDomain(), nil
}

// TempIMPU returns the temporary IMS Public Identity derived from the IMSI
// (TS 23.003 §13.4): sip:<IMSI>@<home-domain>.
func (p PLMN) TempIMPU(imsi string) (string, error) {
	if err := validateDigits(imsi); err != nil {
		return "", fmt.Errorf("imsi: %w", err)
	}
	return "sip:" + imsi + "@" + p.HomeDomain(), nil
}

// MSISDNIdentities returns the (SIP, tel) public identities for an MSISDN.
// The SIP form matches how the deployed S-CSCF builds the P-Associated-URI:
// sip:<msisdn>@<home-domain>. The tel form is the E.164 URI tel:+<msisdn>.
func (p PLMN) MSISDNIdentities(msisdn string) (sip, tel string, err error) {
	m := strings.TrimPrefix(msisdn, "+")
	if err := validateDigits(m); err != nil {
		return "", "", fmt.Errorf("msisdn: %w", err)
	}
	sip = "sip:" + m + "@" + p.HomeDomain()
	tel = "tel:+" + m
	return sip, tel, nil
}

// GPSI returns the SD-Core GPSI form for an MSISDN (msisdn-<digits>), matching
// how the IOS-MCN webconsole stores it in UDR am-data.
func GPSI(msisdn string) (string, error) {
	m := strings.TrimPrefix(strings.TrimPrefix(msisdn, "+"), "msisdn-")
	if err := validateDigits(m); err != nil {
		return "", fmt.Errorf("msisdn: %w", err)
	}
	return "msisdn-" + m, nil
}

// MSISDNFromGPSI extracts the bare MSISDN from a GPSI (msisdn-<digits>).
func MSISDNFromGPSI(gpsi string) (string, error) {
	if !strings.HasPrefix(gpsi, "msisdn-") {
		return "", fmt.Errorf("gpsi %q missing msisdn- prefix", gpsi)
	}
	m := strings.TrimPrefix(gpsi, "msisdn-")
	if err := validateDigits(m); err != nil {
		return "", fmt.Errorf("gpsi digits: %w", err)
	}
	return m, nil
}

func validateDigits(s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return fmt.Errorf("non-digit in %q", s)
		}
	}
	return nil
}
