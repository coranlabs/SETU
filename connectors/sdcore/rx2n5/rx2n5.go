// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package rx2n5 translates a Diameter Rx AA-Request (3GPP TS 29.214, from
// Kamailio ims_qos acting as the AF) into an N5 Npcf_PolicyAuthorization
// AppSessionContext (3GPP TS 29.514). Field shapes follow the 3GPP OpenAPI; the
// SD-Core dialect quirks (flowUsage omission, bandwidth defaults, GBR:=MBR) were
// established on the wire against the live deployment and are pinned by the
// characterization tests in this package.
package rx2n5

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/coranlabs/SETU/wire/diameter"
)

// Rx AVP codes (3GPP TS 29.214, vendor 10415) and base-protocol codes.
const (
	avpFramedIPAddress    uint32 = 8   // base
	avpFramedIPv6Prefix   uint32 = 97  // base
	avpSessionID          uint32 = 263 // base
	avpSubscriptionID     uint32 = 443 // base (grouped)
	avpSubscriptionIDData uint32 = 444 // base
	avpSubscriptionIDType uint32 = 450 // base

	avpAFApplicationID    uint32 = 504
	avpFlowDescription    uint32 = 507
	avpFlowNumber         uint32 = 509
	avpFlowStatus         uint32 = 511
	avpFlowUsage          uint32 = 512
	avpMaxReqBandwidthDL  uint32 = 515
	avpMaxReqBandwidthUL  uint32 = 516
	avpMediaComponentDesc uint32 = 517 // grouped
	avpMediaComponentNum  uint32 = 518
	avpMediaSubComponent  uint32 = 519 // grouped
	avpMediaType          uint32 = 520
)

// Subscription-Id-Type values (RFC 4006).
const (
	subIDEndUserE164 = 0
	subIDEndUserSIP  = 2
)

// Config carries the gateway-supplied fields the AAR does not contain.
type Config struct {
	DNN        string // "ims"
	AfAppID    string // MMTel ICSI (default if the AAR omits AF-Application-Identifier)
	NotifURI   string // AF notification endpoint advertised to the PCF
	SuppFeat   string // supported-features bitmap, e.g. "5"
	SponStatus string // e.g. "SPONSOR_DISABLED"
}

// DefaultConfig is the proven SD-Core dialect (see dialects/sdcore.json).
func DefaultConfig() Config {
	return Config{
		DNN:        "ims",
		AfAppID:    `+g.3gpp.icsi-ref="urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel"`,
		NotifURI:   "http://127.0.0.201:7777",
		SuppFeat:   "5",
		SponStatus: "SPONSOR_DISABLED",
	}
}

// ---- N5 AppSessionContext types (mirror the real payload the PCF accepts) ----

type AppSessionContext struct {
	AscReqData AscReqData `json:"ascReqData"`
}

type AscReqData struct {
	AfAppID       string                    `json:"afAppId"`
	DNN           string                    `json:"dnn"`
	MedComponents map[string]MediaComponent `json:"medComponents"`
	EvSubsc       EvSubsc                   `json:"evSubsc"`
	NotifURI      string                    `json:"notifUri"`
	SponStatus    string                    `json:"sponStatus"`
	Gpsi          string                    `json:"gpsi"`
	SuppFeat      string                    `json:"suppFeat"`
	UeIPv4        string                    `json:"ueIpv4,omitempty"`
	UeIPv6        string                    `json:"ueIpv6Prefix,omitempty"`
}

type MediaComponent struct {
	MedCompN     int                     `json:"medCompN"`
	QosReference string                  `json:"qosReference,omitempty"`
	MedType      string                  `json:"medType"`
	MarBwUl      string                  `json:"marBwUl,omitempty"`
	MarBwDl      string                  `json:"marBwDl,omitempty"`
	MirBwUl      string                  `json:"mirBwUl,omitempty"`
	MirBwDl      string                  `json:"mirBwDl,omitempty"`
	MedSubComps  map[string]MediaSubComp `json:"medSubComps"`
}

type MediaSubComp struct {
	FNum      int      `json:"fNum"`
	FDescs    []string `json:"fDescs"`
	FStatus   string   `json:"fStatus"`
	MarBwDl   string   `json:"marBwDl,omitempty"`
	MarBwUl   string   `json:"marBwUl,omitempty"`
	FlowUsage string   `json:"flowUsage,omitempty"`
}

type EvSubsc struct {
	Events []Event `json:"events"`
	// NotifUri is where the PCF sends event notifications (QoS notif, etc.).
	// The SD-Core PCF reads the event target from evSubsc.notifUri and panics if
	// it is absent, so it must be set (to the same per-session AF notif URL).
	NotifUri string `json:"notifUri,omitempty"`
}

type Event struct {
	Event       string `json:"event"`
	NotifMethod string `json:"notifMethod"`
}

// GPSIResolver maps an IMPU (from the Rx Subscription-Id) to an SD-Core GPSI.
// For an E.164 subscription-id the MSISDN is already present; for a SIP URI whose
// user part is numeric it is used directly; otherwise the resolver (e.g. a UDR
// lookup) is consulted.
type GPSIResolver func(impu string) (string, error)

// Translate converts a decoded Rx AAR into an N5 AppSessionContext.
func Translate(aar diameter.Message, cfg Config, resolve GPSIResolver) (AppSessionContext, error) {
	if aar.Code != diameter.CmdAA || !aar.IsRequest() {
		return AppSessionContext{}, fmt.Errorf("rx2n5: not an AA-Request (code=%d req=%v)", aar.Code, aar.IsRequest())
	}
	var asc AscReqData
	asc.DNN = cfg.DNN
	asc.NotifURI = cfg.NotifURI
	asc.SuppFeat = cfg.SuppFeat
	asc.SponStatus = cfg.SponStatus
	asc.EvSubsc = EvSubsc{Events: []Event{
		{Event: "QOS_NOTIF", NotifMethod: "PERIODIC"},
		{Event: "ANI_REPORT", NotifMethod: "ONE_TIME"},
	}}

	// AF-Application-Identifier
	asc.AfAppID = cfg.AfAppID
	if a, err := diameter.Find(aar.AVPs, avpAFApplicationID, diameter.Vendor3GPP); err == nil {
		asc.AfAppID = a.StrVal()
	}

	// UE IP (Framed-IP-Address IPv4, else Framed-IPv6-Prefix)
	if a, err := diameter.Find(aar.AVPs, avpFramedIPAddress, 0); err == nil {
		ip, err := a.IPVal()
		if err != nil {
			return AppSessionContext{}, fmt.Errorf("rx2n5: bad Framed-IP-Address: %w", err)
		}
		asc.UeIPv4 = ip.String()
	} else if a, err := diameter.Find(aar.AVPs, avpFramedIPv6Prefix, 0); err == nil {
		asc.UeIPv6 = ipv6PrefixString(a.Data)
	} else {
		return AppSessionContext{}, fmt.Errorf("rx2n5: AAR has no UE IP (Framed-IP-Address/Framed-IPv6-Prefix)")
	}

	// GPSI from Subscription-Id (best-effort). Terminating IMS AARs often carry only
	// the callee CONTACT URI (sip:<uuid>@<ip>) as Subscription-Id, which has no numeric
	// MSISDN to resolve. That is NOT fatal: Framed-IP-Address already pins the UE and
	// PCF SessionBinding matches the SM policy by ueIpv4 (the SMF stamps Ipv4Address into
	// every SM policy). Dropping the AAR here lost the callee dedicated bearer entirely
	// (-> a=curr:qos local none -> SIP 580). Fall back to binding by IP.
	if gpsi, gerr := gpsiFromSubscriptionID(aar.AVPs, resolve); gerr == nil {
		asc.Gpsi = gpsi
	} else {
		asc.Gpsi = ""
	}

	// Media-Component-Description(s)
	comps := diameter.FindAll(aar.AVPs, avpMediaComponentDesc, diameter.Vendor3GPP)
	if len(comps) == 0 {
		return AppSessionContext{}, fmt.Errorf("rx2n5: AAR has no Media-Component-Description")
	}
	asc.MedComponents = map[string]MediaComponent{}
	for i, c := range comps {
		mc, err := translateMediaComponent(c)
		if err != nil {
			return AppSessionContext{}, err
		}
		asc.MedComponents[strconv.Itoa(i)] = mc
	}
	return AppSessionContext{AscReqData: asc}, nil
}

func translateMediaComponent(c diameter.AVP) (MediaComponent, error) {
	kids, err := c.SubAVPs()
	if err != nil {
		return MediaComponent{}, err
	}
	var mc MediaComponent
	if a, err := diameter.Find(kids, avpMediaComponentNum, diameter.Vendor3GPP); err == nil {
		n, _ := a.U32Val()
		mc.MedCompN = int(n)
	}
	medType := uint32(0)
	if a, err := diameter.Find(kids, avpMediaType, diameter.Vendor3GPP); err == nil {
		medType, _ = a.U32Val()
	}
	mc.MedType = mediaTypeName(medType)
	mc.QosReference = qosReferenceFor(medType)

	// Component-level Max-Requested-Bandwidth (TS 29.214). The SD-Core PCF derives
	// the QoS flow MBR/GBR from the *component-level* bandwidth (comp.GetMarBwUl /
	// GetMirBwUl), not the sub-component. A 5QI-1 GBR voice flow with 0 GBR is an
	// invalid bearer the UE rejects with SIP 580 Precondition Failure, so read the
	// AAR value here and default AUDIO to a valid voice bitrate when absent.
	if a, err := diameter.Find(kids, avpMaxReqBandwidthUL, diameter.Vendor3GPP); err == nil {
		if v, err := a.U32Val(); err == nil {
			mc.MarBwUl = kbps(v)
		}
	}
	if a, err := diameter.Find(kids, avpMaxReqBandwidthDL, diameter.Vendor3GPP); err == nil {
		if v, err := a.U32Val(); err == nil {
			mc.MarBwDl = kbps(v)
		}
	}
	if mc.MedType == "AUDIO" || mc.MedType == "VIDEO" {
		def := "64 Kbps" // 5QI-1 conversational voice
		if mc.MedType == "VIDEO" {
			def = "384 Kbps" // 5QI-2 conversational video
		}
		if mc.MarBwUl == "" {
			mc.MarBwUl = def
		}
		if mc.MarBwDl == "" {
			mc.MarBwDl = def
		}
		// GBR == MBR so the guaranteed rate never exceeds the max (5QI-1 voice / 5QI-2 video).
		// The video component used to carry only marBw (no mirBw) -> GBR 0 -> the 5QI-2
		// video flow was invalid and the Rx AAR failed with 488 "no QoS available".
		mc.MirBwUl = mc.MarBwUl
		mc.MirBwDl = mc.MarBwDl
	}

	subs := diameter.FindAll(kids, avpMediaSubComponent, diameter.Vendor3GPP)
	if len(subs) == 0 {
		return MediaComponent{}, fmt.Errorf("rx2n5: media component %d has no sub-components", mc.MedCompN)
	}
	mc.MedSubComps = map[string]MediaSubComp{}
	for i, s := range subs {
		sc, err := translateSubComponent(s)
		if err != nil {
			return MediaComponent{}, err
		}
		mc.MedSubComps[strconv.Itoa(i)] = sc
	}
	return mc, nil
}

func translateSubComponent(s diameter.AVP) (MediaSubComp, error) {
	kids, err := s.SubAVPs()
	if err != nil {
		return MediaSubComp{}, err
	}
	var sc MediaSubComp
	if a, err := diameter.Find(kids, avpFlowNumber, diameter.Vendor3GPP); err == nil {
		n, _ := a.U32Val()
		sc.FNum = int(n)
	}
	for _, fd := range diameter.FindAll(kids, avpFlowDescription, diameter.Vendor3GPP) {
		sc.FDescs = append(sc.FDescs, fd.StrVal())
	}
	if len(sc.FDescs) == 0 {
		return MediaSubComp{}, fmt.Errorf("rx2n5: sub-component %d has no Flow-Description", sc.FNum)
	}
	fs := uint32(2) // default ENABLED
	if a, err := diameter.Find(kids, avpFlowStatus, diameter.Vendor3GPP); err == nil {
		fs, _ = a.U32Val()
	}
	sc.FStatus = flowStatusName(fs)
	fu := uint32(0)
	if a, err := diameter.Find(kids, avpFlowUsage, diameter.Vendor3GPP); err == nil {
		fu, _ = a.U32Val()
	}
	sc.FlowUsage = flowUsageName(fu)
	if a, err := diameter.Find(kids, avpMaxReqBandwidthUL, diameter.Vendor3GPP); err == nil {
		v, _ := a.U32Val()
		sc.MarBwUl = kbps(v)
	}
	if a, err := diameter.Find(kids, avpMaxReqBandwidthDL, diameter.Vendor3GPP); err == nil {
		v, _ := a.U32Val()
		sc.MarBwDl = kbps(v)
	}
	return sc, nil
}

func gpsiFromSubscriptionID(avps []diameter.AVP, resolve GPSIResolver) (string, error) {
	for _, sub := range diameter.FindAll(avps, avpSubscriptionID, 0) {
		kids, err := sub.SubAVPs()
		if err != nil {
			continue
		}
		typ := uint32(subIDEndUserSIP)
		if a, err := diameter.Find(kids, avpSubscriptionIDType, 0); err == nil {
			typ, _ = a.U32Val()
		}
		data, err := diameter.Find(kids, avpSubscriptionIDData, 0)
		if err != nil {
			continue
		}
		val := data.StrVal()
		switch typ {
		case subIDEndUserE164:
			return "msisdn-" + strings.TrimPrefix(val, "+"), nil
		case subIDEndUserSIP:
			// A SIP IMPU's user part may be an MSISDN OR an IMSI (temporary IMPU
			// sip:<imsi>@domain). Only a resolver can tell them apart and map to
			// the right GPSI; fall back to msisdn-<user> only when no resolver.
			if resolve != nil {
				if g, err := resolve(val); err == nil {
					return g, nil
				}
			}
			user := sipUserPart(val)
			if isNumeric(user) && len(user) != 15 { // 15 digits => IMSI, not MSISDN
				return "msisdn-" + user, nil
			}
		}
	}
	return "", fmt.Errorf("rx2n5: no usable Subscription-Id in AAR")
}

// ---- value mappings ----

func mediaTypeName(t uint32) string {
	switch t {
	case 0:
		return "AUDIO"
	case 1:
		return "VIDEO"
	case 2:
		return "DATA"
	case 3:
		return "APPLICATION"
	case 4:
		return "CONTROL"
	case 5:
		return "TEXT"
	case 6:
		return "MESSAGE"
	default:
		return "OTHER"
	}
}

// qosReferenceFor emits the deployment's QoS hint (audio->qosVoNR, video->qosViNR); the
// PCF ignores it (it derives 5QI from medType: AUDIO->1, VIDEO->2).
func qosReferenceFor(t uint32) string {
	switch t {
	case 0:
		return "qosVoNR"
	case 1:
		return "qosViNR"
	default:
		return ""
	}
}

func flowStatusName(s uint32) string {
	switch s {
	case 0:
		return "ENABLED_UPLINK"
	case 1:
		return "ENABLED_DOWNLINK"
	case 2:
		return "ENABLED"
	case 3:
		return "DISABLED"
	case 4:
		return "REMOVED"
	default:
		return "ENABLED"
	}
}

func flowUsageName(u uint32) string {
	switch u {
	case 1:
		return "RTCP"
	case 2:
		return "AF_SIGNALLING"
	default:
		// 3GPP TS 29.514: flowUsage is optional and "no information" is the
		// default, so omit the field. Stock OMEC PCF rejects the explicit
		// "NO_INFORMATION" literal ("not a valid FlowUsage").
		return ""
	}
}

// kbps converts a bit-rate in bit/s (as carried in the Rx bandwidth AVPs) to the
// "<n> Kbps" string form the PCF expects.
func kbps(bps uint32) string {
	return strconv.Itoa(int(bps/1000)) + " Kbps"
}

func sipUserPart(uri string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(uri, "sip:"), "sips:")
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, "+")
}

func isNumeric(s string) bool {
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

func ipv6PrefixString(data []byte) string {
	// Framed-IPv6-Prefix: 1 byte reserved, 1 byte prefix-length, then prefix bytes.
	if len(data) < 2 {
		return ""
	}
	plen := int(data[1])
	ipBytes := make([]byte, 16)
	copy(ipBytes, data[2:])
	return net.IP(ipBytes).String() + "/" + strconv.Itoa(plen)
}

// SessionID extracts the Diameter Session-Id from a message (used to correlate an
// STR to the app-session to DELETE).
func SessionID(m diameter.Message) (string, error) {
	a, err := diameter.Find(m.AVPs, avpSessionID, 0)
	if err != nil {
		return "", fmt.Errorf("rx2n5: no Session-Id")
	}
	return a.StrVal(), nil
}

// sortedKeys is a helper used by callers that need deterministic ordering.
func sortedKeys(m map[string]MediaComponent) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

var _ = sortedKeys

// ---- Rx message builders (as Kamailio ims_qos constructs them; reusable by an
// AF client, a load generator, and the gateway's end-to-end tests) ----

func mediaSubComponent(fnum uint32, fdescs []string, flowStatus, flowUsage, bwUL, bwDL uint32) diameter.AVP {
	kids := []diameter.AVP{diameter.U32(avpFlowNumber, diameter.Vendor3GPP, true, fnum)}
	for _, fd := range fdescs {
		kids = append(kids, diameter.Str(avpFlowDescription, diameter.Vendor3GPP, true, fd))
	}
	kids = append(kids,
		diameter.U32(avpFlowStatus, diameter.Vendor3GPP, true, flowStatus),
		diameter.U32(avpFlowUsage, diameter.Vendor3GPP, true, flowUsage),
		diameter.U32(avpMaxReqBandwidthUL, diameter.Vendor3GPP, true, bwUL),
		diameter.U32(avpMaxReqBandwidthDL, diameter.Vendor3GPP, true, bwDL),
	)
	return diameter.Grouped(avpMediaSubComponent, diameter.Vendor3GPP, true, kids...)
}

func flowPair(ueIP string, port int) []string {
	p := strconv.Itoa(port)
	return []string{
		"permit out 17 from any to " + ueIP + " " + p,
		"permit in 17 from " + ueIP + " " + p + " to any",
	}
}

func audioComp(ueIP string, audioPort, rtcpPort int) diameter.AVP {
	return diameter.Grouped(avpMediaComponentDesc, diameter.Vendor3GPP, true,
		diameter.U32(avpMediaComponentNum, diameter.Vendor3GPP, true, 1),
		diameter.U32(avpMediaType, diameter.Vendor3GPP, true, 0),
		mediaSubComponent(1, flowPair(ueIP, audioPort), 2, 0, 128000, 128000),
		mediaSubComponent(2, flowPair(ueIP, rtcpPort), 2, 1, 128000, 128000),
	)
}

func videoComp(ueIP string, videoPort, rtcpPort int) diameter.AVP {
	return diameter.Grouped(avpMediaComponentDesc, diameter.Vendor3GPP, true,
		diameter.U32(avpMediaComponentNum, diameter.Vendor3GPP, true, 2),
		diameter.U32(avpMediaType, diameter.Vendor3GPP, true, 1),
		mediaSubComponent(1, flowPair(ueIP, videoPort), 2, 0, 2048000, 2048000),
		mediaSubComponent(2, flowPair(ueIP, rtcpPort), 2, 1, 128000, 128000),
	)
}

func e164SubID(msisdn string) diameter.AVP {
	return diameter.Grouped(avpSubscriptionID, 0, true,
		diameter.U32(avpSubscriptionIDType, 0, true, subIDEndUserE164),
		diameter.Str(avpSubscriptionIDData, 0, true, msisdn),
	)
}

func aarBase(sessionID, ueIP, msisdn string, comps ...diameter.AVP) diameter.Message {
	avps := []diameter.AVP{
		diameter.Str(avpSessionID, 0, true, sessionID),
		diameter.Str(avpAFApplicationID, diameter.Vendor3GPP, true,
			`+g.3gpp.icsi-ref="urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel"`),
		diameter.FramedIP(avpFramedIPAddress, 0, true, net.ParseIP(ueIP)),
		e164SubID(msisdn),
	}
	avps = append(avps, comps...)
	return diameter.Message{Flags: diameter.CmdRequest | diameter.CmdProxiable, Code: diameter.CmdAA, AppID: diameter.AppRx, AVPs: avps}
}

// BuildAudioAAR builds a VoNR (audio) Rx AA-Request.
func BuildAudioAAR(sessionID, ueIP, msisdn string, audioPort, rtcpPort int) diameter.Message {
	return aarBase(sessionID, ueIP, msisdn, audioComp(ueIP, audioPort, rtcpPort))
}

// BuildVideoAAR builds a ViNR (audio+video) Rx AA-Request.
func BuildVideoAAR(sessionID, ueIP, msisdn string, audioPort, audioRTCP, videoPort, videoRTCP int) diameter.Message {
	return aarBase(sessionID, ueIP, msisdn, audioComp(ueIP, audioPort, audioRTCP), videoComp(ueIP, videoPort, videoRTCP))
}

// BuildSTR builds a Session-Termination-Request for the given Rx session.
func BuildSTR(sessionID string) diameter.Message {
	return diameter.Message{
		Flags: diameter.CmdRequest | diameter.CmdProxiable, Code: diameter.CmdST, AppID: diameter.AppRx,
		AVPs: []diameter.AVP{
			diameter.Str(avpSessionID, 0, true, sessionID),
			diameter.U32(295, 0, true, 1), // Termination-Cause = DIAMETER_LOGOUT
		},
	}
}
