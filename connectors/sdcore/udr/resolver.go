// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package udr

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GPSIResolver returns a resolver that maps a SIP/tel IMPU to an SD-Core GPSI by
// asking the UDR. It handles the two real cases:
//   - a temporary IMPU  sip:<IMSI>@domain  -> look up am-data by imsi-<IMSI> -> msisdn-<MSISDN>
//   - an MSISDN IMPU     sip:<MSISDN>@domain / tel:+<MSISDN> -> msisdn-<MSISDN> directly
//
// An alphanumeric IMPU with no numeric identity returns an error (no GPSI ⇒ the PCF
// cannot bind ⇒ the gateway will reject the AAR rather than bind the wrong policy).
func GPSIResolver(udrBase, plmnID string, httpc *http.Client) func(string) (string, error) {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	base := strings.TrimRight(udrBase, "/")
	return func(impu string) (string, error) {
		user := sipUser(impu)
		if !isDigits(user) {
			return "", fmt.Errorf("udr: IMPU %q has no numeric identity to resolve to a GPSI", impu)
		}
		if len(user) == 15 { // IMSI -> resolve MSISDN via UDR am-data
			url := fmt.Sprintf("%s/nudr-dr/v2/subscription-data/imsi-%s/%s/provisioned-data/am-data", base, user, plmnID)
			resp, err := httpc.Get(url)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				return "", fmt.Errorf("udr: am-data status %d for imsi-%s", resp.StatusCode, user)
			}
			msisdn, err := ParseMSISDN(body)
			if err != nil {
				return "", err
			}
			return "msisdn-" + msisdn, nil
		}
		return "msisdn-" + user, nil // already an MSISDN
	}
}

func sipUser(uri string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(uri, "sip:"), "sips:")
	s = strings.TrimPrefix(s, "tel:")
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, "+")
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
