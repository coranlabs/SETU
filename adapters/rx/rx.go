// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package rx terminates Diameter Rx (3GPP TS 29.214) from the P-CSCF and drives
// the configured CoreConnector to authorize media.
package rx

import (
	"context"
	"errors"
	"github.com/coranlabs/SETU/uilog"
	"log"
	"net"
	"time"

	api "github.com/coranlabs/SETU/api/v1"
	"github.com/coranlabs/SETU/fabric"
	"github.com/coranlabs/SETU/wire/diameter"
	"github.com/coranlabs/SETU/wire/dpeer"
)

type Adapter struct {
	Listen      string
	OriginHost  string
	OriginRealm string
	HostIP      string
	Core        api.CoreConnector
	Grants      *fabric.Grants
	// Strict answers real error codes when the core rejects or is unreachable.
	// When false the adapter answers 2001 regardless, which is what the P-CSCF
	// expects from the deployment this replaced.
	Strict  bool
	Timeout time.Duration // per-core-call budget; default 25s
}

// Handle answers AAR and STR. Everything else gets the standard answer AVPs.
func (a *Adapter) Handle(_ *dpeer.Conn, req diameter.Message) (diameter.Message, error) {
	ans := req.Answer()
	var avps []diameter.AVP
	sid := ""
	if s, err := diameter.Find(req.AVPs, diameter.AVPSessionID, 0); err == nil {
		avps = append(avps, s)
		sid = s.StrVal()
	}
	result := resultAVPs(diameter.ResultCode(2001))

	switch req.Code {
	case diameter.CmdAA:
		ctx, cancel := context.WithTimeout(context.Background(), a.timeout())
		ref, err := a.Core.Policy().Create(ctx, &api.MediaGrant{ID: sid, UE: ueKey(req), Raw: req})
		cancel()
		if err != nil {
			uilog.Event(uilog.Red, "⛔", "CALL-FAIL", "ue %s  dedicated bearer NOT installed: %v", ueKey(req).IPv4, err)
			if a.Strict {
				result = strictAnswer(err)
			} // otherwise fall through and answer 2001
		} else {
			if err := a.Grants.Bind(sid, ref); err != nil {
				uilog.Event(uilog.Orange, "⚠", "WAL-WARN", "grant persist failed: %v", err)
			}
			ue := ueKey(req).IPv4
			sess := uilog.Short(ref)
			mts := mediaTypes(req)
			if len(mts) == 0 {
				uilog.Event(uilog.Green, "📶", "CALL-UP", "ue %-15s  dedicated GBR bearer installed  · sess %s", ue, sess)
			}
			for _, mt := range mts {
				switch mt {
				case "AUDIO":
					uilog.Event(uilog.Green, "🎧", "AUDIO", "ue %-15s  5QI-1 GBR voice  ✓ bearer UP  · sess %s", ue, sess)
				case "VIDEO":
					uilog.Event(uilog.Cyan, "🎥", "VIDEO", "ue %-15s  5QI-2 GBR video  ✓ bearer UP  · sess %s", ue, sess)
				default:
					uilog.Event(uilog.Blue, "📡", "MEDIA", "ue %-15s  %s bearer UP  · sess %s", ue, mt, sess)
				}
			}
		}
	case diameter.CmdST:
		if ref, ok := a.Grants.Take(sid); ok {
			ctx, cancel := context.WithTimeout(context.Background(), a.timeout())
			if err := a.Core.Policy().Delete(ctx, ref); err != nil {
				uilog.Event(uilog.Red, "⛔", "END-FAIL", "sess %s  release failed: %v", uilog.Short(ref), err)
			} else {
				uilog.Event(uilog.Orange, "📴", "CALL-END", "sess %-22s  dedicated bearer released", uilog.Short(ref))
			}
			cancel()
		}
	}

	avps = append(avps,
		diameter.U32(diameter.AVPAuthApplicationID, 0, true, diameter.AppRx),
		diameter.U32(diameter.AVPAuthSessionState, 0, true, 0),
		diameter.OriginHost(a.OriginHost),
		diameter.OriginRealm(a.OriginRealm),
	)
	avps = append(avps, result...)
	ans.AVPs = avps
	return ans, nil
}

// strictAnswer maps connector errors onto TS 29.214 result codes.
func strictAnswer(err error) []diameter.AVP {
	switch {
	case errors.Is(err, api.ErrNoSession):
		return resultAVPs(experimentalResult(5065)) // IP_CAN_SESSION_NOT_AVAILABLE
	case errors.Is(err, api.ErrRejected):
		return resultAVPs(experimentalResult(5063)) // REQUESTED_SERVICE_NOT_AUTHORIZED
	default: // ErrUnavailable and anything else
		return resultAVPs(diameter.ResultCode(3004)) // DIAMETER_TOO_BUSY
	}
}

func resultAVPs(a diameter.AVP) []diameter.AVP { return []diameter.AVP{a} }

// experimentalResult builds Experimental-Result(297){Vendor-Id(266)=3GPP,
// Experimental-Result-Code(298)=code}.
func experimentalResult(code uint32) diameter.AVP {
	return diameter.Grouped(297, 0, true,
		diameter.U32(266, 0, true, diameter.Vendor3GPP),
		diameter.U32(298, 0, true, code),
	)
}

// mediaTypes lists the distinct media in an AAR, from
// Media-Component-Description(517) -> Media-Type(520).
func mediaTypes(req diameter.Message) []string {
	const mediaCompDesc, mediaType uint32 = 517, 520
	var out []string
	seen := map[string]bool{}
	for _, c := range diameter.FindAll(req.AVPs, mediaCompDesc, diameter.Vendor3GPP) {
		kids, err := c.SubAVPs()
		if err != nil {
			continue
		}
		name := "AUDIO"
		if a, err := diameter.Find(kids, mediaType, diameter.Vendor3GPP); err == nil {
			if v, err := a.U32Val(); err == nil {
				switch v {
				case 1:
					name = "VIDEO"
				case 2:
					name = "DATA"
				default:
					name = "AUDIO"
				}
			}
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func ueKey(req diameter.Message) api.UEKey {
	k := api.UEKey{}
	if a, err := diameter.Find(req.AVPs, 8, 0); err == nil { // Framed-IP-Address
		if ip, e := a.IPVal(); e == nil {
			k.IPv4 = ip.String()
		}
	}
	return k
}

func (a *Adapter) logAAR(sid string, req diameter.Message) {
	fip := "none"
	if av, err := diameter.Find(req.AVPs, 8, 0); err == nil {
		if ip, e := av.IPVal(); e == nil {
			fip = ip.String()
		}
	}
	var subs []string
	for _, sub := range diameter.FindAll(req.AVPs, 443, 0) {
		if kids, e := sub.SubAVPs(); e == nil {
			if d, e2 := diameter.Find(kids, 444, 0); e2 == nil {
				subs = append(subs, d.StrVal())
			}
		}
	}
	log.Printf("rx   Rx-AAR         session=%s  ue=%s  ids=%v  (P-CSCF requesting bearer)", sid, fip, subs)
}

func (a *Adapter) timeout() time.Duration {
	if a.Timeout == 0 {
		return 25 * time.Second
	}
	return a.Timeout
}

// Serve runs the Diameter Rx server until it fails.
func (a *Adapter) Serve() error {
	srv := &dpeer.Server{
		ID: dpeer.Identity{
			OriginHost:  a.OriginHost,
			OriginRealm: a.OriginRealm,
			HostIP:      net.ParseIP(a.HostIP),
			ProductName: "setu-rx",
			VendorID:    diameter.Vendor3GPP,
			AppIDs:      []uint32{diameter.AppRx},
		},
		Handler: a.Handle,
	}
	if err := srv.Listen(a.Listen); err != nil {
		return err
	}
	log.Printf("rx: Diameter Rx on %s -> core %q (strict=%v, grants=%d restored)",
		srv.Addr(), a.Core.Name(), a.Strict, a.Grants.Len())
	return srv.Serve()
}
