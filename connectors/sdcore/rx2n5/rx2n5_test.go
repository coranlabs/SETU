// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package rx2n5

import (
	"encoding/json"
	"net"
	"reflect"
	"testing"

	"github.com/coranlabs/SETU/wire/diameter"
)

// ---- real AAR builders (as Kamailio ims_qos constructs them) ----

func subComp(fnum uint32, fdescs []string, flowStatus, flowUsage, bwUL, bwDL uint32) diameter.AVP {
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

func audioComponent(ueIP string, audioPort, rtcpPort int) diameter.AVP {
	rtp := subComp(1, []string{
		"permit out 17 from any to " + ueIP + " " + itoa(audioPort),
		"permit in 17 from " + ueIP + " " + itoa(audioPort) + " to any",
	}, 2, 0, 128000, 128000)
	rtcp := subComp(2, []string{
		"permit out 17 from any to " + ueIP + " " + itoa(rtcpPort),
		"permit in 17 from " + ueIP + " " + itoa(rtcpPort) + " to any",
	}, 2, 1, 128000, 128000)
	return diameter.Grouped(avpMediaComponentDesc, diameter.Vendor3GPP, true,
		diameter.U32(avpMediaComponentNum, diameter.Vendor3GPP, true, 1),
		diameter.U32(avpMediaType, diameter.Vendor3GPP, true, 0), // AUDIO
		rtp, rtcp,
	)
}

func videoComponent(ueIP string, videoPort, rtcpPort int) diameter.AVP {
	rtp := subComp(1, []string{
		"permit out 17 from any to " + ueIP + " " + itoa(videoPort),
		"permit in 17 from " + ueIP + " " + itoa(videoPort) + " to any",
	}, 2, 0, 2048000, 2048000)
	rtcp := subComp(2, []string{
		"permit out 17 from any to " + ueIP + " " + itoa(rtcpPort),
		"permit in 17 from " + ueIP + " " + itoa(rtcpPort) + " to any",
	}, 2, 1, 128000, 128000)
	return diameter.Grouped(avpMediaComponentDesc, diameter.Vendor3GPP, true,
		diameter.U32(avpMediaComponentNum, diameter.Vendor3GPP, true, 2),
		diameter.U32(avpMediaType, diameter.Vendor3GPP, true, 1), // VIDEO
		rtp, rtcp,
	)
}

func subscriptionIDE164(msisdn string) diameter.AVP {
	return diameter.Grouped(avpSubscriptionID, 0, true,
		diameter.U32(avpSubscriptionIDType, 0, true, subIDEndUserE164),
		diameter.Str(avpSubscriptionIDData, 0, true, msisdn),
	)
}

func baseAAR(ueIP, msisdn string, comps ...diameter.AVP) diameter.Message {
	avps := []diameter.AVP{
		diameter.Str(avpSessionID, 0, true, "pcscf.ims;1;1;aar"),
		diameter.Str(avpAFApplicationID, diameter.Vendor3GPP, true,
			`+g.3gpp.icsi-ref="urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel"`),
		diameter.FramedIP(avpFramedIPAddress, 0, true, net.ParseIP(ueIP)),
		subscriptionIDE164(msisdn),
	}
	avps = append(avps, comps...)
	return diameter.Message{Flags: diameter.CmdRequest | diameter.CmdProxiable, Code: diameter.CmdAA, AppID: diameter.AppRx, HopByHop: 1, EndToEnd: 1, AVPs: avps}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// Encoding then decoding the AAR (real wire bytes) must translate to the exact
// N5 AppSessionContext structure.
func TestTranslateAudioAAR(t *testing.T) {
	aar := baseAAR("10.45.0.2", "9000000001", audioComponent("10.45.0.2", 5000, 5001))
	// go through real wire bytes to prove the codec + translator pipeline
	decoded, err := diameter.DecodeMessage(aar.Encode())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Translate(decoded, DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}

	want := AppSessionContext{AscReqData: AscReqData{
		AfAppID: `+g.3gpp.icsi-ref="urn%3Aurn-7%3A3gpp-service.ims.icsi.mmtel"`,
		DNN:     "ims",
		MedComponents: map[string]MediaComponent{
			"0": {
				MedCompN: 1, QosReference: "qosVoNR", MedType: "AUDIO",
				// Component-level bandwidth: the AAR carries none, so the audio
				// default (64 Kbps) applies and GBR:=MBR — the e2e-PROVEN behaviour
				// of the deployed gateway (zero-GBR 5QI-1 ⇒ SIP 580; see docs/02).
				MarBwUl: "64 Kbps", MarBwDl: "64 Kbps",
				MirBwUl: "64 Kbps", MirBwDl: "64 Kbps",
				MedSubComps: map[string]MediaSubComp{
					"0": {FNum: 1, FDescs: []string{
						"permit out 17 from any to 10.45.0.2 5000",
						"permit in 17 from 10.45.0.2 5000 to any",
					}, FStatus: "ENABLED", MarBwDl: "128 Kbps", MarBwUl: "128 Kbps", FlowUsage: ""},
					"1": {FNum: 2, FDescs: []string{
						"permit out 17 from any to 10.45.0.2 5001",
						"permit in 17 from 10.45.0.2 5001 to any",
					}, FStatus: "ENABLED", MarBwDl: "128 Kbps", MarBwUl: "128 Kbps", FlowUsage: "RTCP"},
				},
			},
		},
		EvSubsc:    EvSubsc{Events: []Event{{"QOS_NOTIF", "PERIODIC"}, {"ANI_REPORT", "ONE_TIME"}}},
		NotifURI:   "http://127.0.0.201:7777",
		SponStatus: "SPONSOR_DISABLED",
		Gpsi:       "msisdn-9000000001",
		SuppFeat:   "5",
		UeIPv4:     "10.45.0.2",
	}}
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.MarshalIndent(got, "", "  ")
		wj, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("audio translate mismatch\nGOT:\n%s\nWANT:\n%s", gj, wj)
	}

	// output must be valid JSON and re-parseable
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("output not JSON-serialisable: %v", err)
	}
}

// A video AAR (audio + video components) must yield the two-component structure
// (5QI-1 audio + 5QI-2 video) matching the real ViNR payload.
func TestTranslateVideoAAR(t *testing.T) {
	aar := baseAAR("10.45.0.9", "9000000002",
		audioComponent("10.45.0.9", 6000, 6001),
		videoComponent("10.45.0.9", 7000, 7001),
	)
	decoded, _ := diameter.DecodeMessage(aar.Encode())
	got, err := Translate(decoded, DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.AscReqData.MedComponents) != 2 {
		t.Fatalf("want 2 media components, got %d", len(got.AscReqData.MedComponents))
	}
	audio := got.AscReqData.MedComponents["0"]
	video := got.AscReqData.MedComponents["1"]
	if audio.MedType != "AUDIO" || audio.MedCompN != 1 {
		t.Fatalf("component 0 = %+v, want AUDIO/1", audio)
	}
	if video.MedType != "VIDEO" || video.MedCompN != 2 {
		t.Fatalf("component 1 = %+v, want VIDEO/2", video)
	}
	if video.MedSubComps["0"].MarBwUl != "2048 Kbps" {
		t.Fatalf("video marBwUl = %q, want 2048 Kbps", video.MedSubComps["0"].MarBwUl)
	}
}

func TestTranslateRejectsNonAAR(t *testing.T) {
	m := diameter.Message{Flags: 0, Code: diameter.CmdAA, AppID: diameter.AppRx}
	if _, err := Translate(m, DefaultConfig(), nil); err == nil {
		t.Fatal("expected rejection of non-request")
	}
	m2 := diameter.Message{Flags: diameter.CmdRequest, Code: diameter.CmdST, AppID: diameter.AppRx}
	if _, err := Translate(m2, DefaultConfig(), nil); err == nil {
		t.Fatal("expected rejection of non-AAR command")
	}
}
