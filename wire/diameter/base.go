// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package diameter

import "net"

// Base-protocol AVP codes (RFC 6733) and 3GPP Cx/Rx codes used by the bridges.
const (
	AVPUserName               uint32 = 1
	AVPHostIPAddress          uint32 = 257
	AVPAuthApplicationID      uint32 = 258
	AVPVendorSpecificAppID    uint32 = 260
	AVPSessionID              uint32 = 263
	AVPOriginHost             uint32 = 264
	AVPSupportedVendorID      uint32 = 265
	AVPVendorID               uint32 = 266
	AVPResultCode             uint32 = 268
	AVPProductName            uint32 = 269
	AVPAuthSessionState       uint32 = 277
	AVPOriginStateID          uint32 = 278
	AVPDestinationRealm       uint32 = 283
	AVPDestinationHost        uint32 = 293
	AVPOriginRealm            uint32 = 296
	AVPExperimentalResult     uint32 = 297
	AVPExperimentalResultCode uint32 = 298

	// 3GPP Cx (vendor 10415)
	AVPPublicIdentity           uint32 = 601
	AVPServerName               uint32 = 602
	AVPUserData                 uint32 = 606
	AVPSIPNumberAuthItems       uint32 = 607
	AVPSIPAuthDataItem          uint32 = 612
	AVPServerAssignmentType     uint32 = 614
	AVPUserDataAlreadyAvailable uint32 = 624
	AVPSIPAuthScheme            uint32 = 608
	AVPSIPAuthenticate          uint32 = 609
	AVPSIPAuthorization         uint32 = 610
	AVPConfidentialityKey       uint32 = 625
	AVPIntegrityKey             uint32 = 626
	AVPVisitedNetworkID         uint32 = 600
)

// Result codes (RFC 6733 §7.1).
const (
	Success                uint32 = 2001
	TooBusy                uint32 = 3004
	AuthenticationRejected uint32 = 4001
	UnknownPeer            uint32 = 5010
	UnableToComply         uint32 = 5012
)

// OriginStateID builds an Origin-State-Id AVP.
func OriginStateID(v uint32) AVP { return U32(AVPOriginStateID, 0, true, v) }

// DestinationHost / DestinationRealm helpers.
func DestinationHost(h string) AVP  { return Str(AVPDestinationHost, 0, true, h) }
func DestinationRealm(r string) AVP { return Str(AVPDestinationRealm, 0, true, r) }

// Auth-Session-State values.
const (
	StateMaintained   uint32 = 0
	NoStateMaintained uint32 = 1
)

// OriginHost / OriginRealm helpers.
func OriginHost(h string) AVP  { return Str(AVPOriginHost, 0, true, h) }
func OriginRealm(r string) AVP { return Str(AVPOriginRealm, 0, true, r) }

// ResultCode builds a Result-Code AVP.
func ResultCode(rc uint32) AVP { return U32(AVPResultCode, 0, true, rc) }

// VendorSpecificApplicationID builds the grouped VSAI AVP for a 3GPP application.
func VendorSpecificApplicationID(appID uint32) AVP {
	return Grouped(AVPVendorSpecificAppID, 0, true,
		U32(AVPVendorID, 0, true, Vendor3GPP),
		U32(AVPAuthApplicationID, 0, true, appID),
	)
}

// HostIPAddress builds a Host-IP-Address AVP (Diameter Address).
func HostIPAddress(ip net.IP) AVP { return Addr(AVPHostIPAddress, 0, true, ip) }
