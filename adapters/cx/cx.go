// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package cx answers Diameter Cx (3GPP TS 29.228/29.229) for the I-CSCF and
// S-CSCF: UAR, LIR, SAR and MAR. Subscriber data and AKA vectors come from the
// configured CoreConnector, so no HSS is required.
package cx

import (
	"context"
	"fmt"
	"github.com/coranlabs/SETU/uilog"
	"log"
	"net"
	"strings"
	"time"

	api "github.com/coranlabs/SETU/api/v1"
	"github.com/coranlabs/SETU/chassis/metrics"
	"github.com/coranlabs/SETU/ims/ident"
	"github.com/coranlabs/SETU/ims/imssub"
	"github.com/coranlabs/SETU/wire/diameter"
	"github.com/coranlabs/SETU/wire/dpeer"
)

type Adapter struct {
	Listen      string
	OriginHost  string
	OriginRealm string
	HostIP      string
	SCSCFName   string // sip: URI returned in UAA/LIA Server-Name
	PLMN        ident.PLMN
	Core        api.CoreConnector
	Timeout     time.Duration

	Metrics                *metrics.Registry
	mSAR, mUAR, mLIR, mErr *metrics.Counter
}

func (a *Adapter) InitMetrics() *metrics.Registry {
	reg := metrics.New()
	a.Metrics = reg
	a.mSAR = reg.Counter("setu_cx_sar_total", "Cx SAR handled")
	a.mUAR = reg.Counter("setu_cx_uar_total", "Cx UAR handled")
	a.mLIR = reg.Counter("setu_cx_lir_total", "Cx LIR handled")
	a.mErr = reg.Counter("setu_cx_errors_total", "Cx request failures")
	return reg
}

func inc(c *metrics.Counter) {
	if c != nil {
		c.Inc()
	}
}

func (a *Adapter) Handle(_ *dpeer.Conn, req diameter.Message) (diameter.Message, error) {
	switch req.Code {
	case diameter.CmdUAR:
		inc(a.mUAR)
		return a.handleUAR(req)
	case diameter.CmdLIR:
		inc(a.mLIR)
		return a.handleLIR(req)
	case diameter.CmdSAR:
		inc(a.mSAR)
		return a.handleSAR(req)
	case diameter.CmdMAR:
		return a.handleMAR(req)
	default:
		return a.answerErr(req, diameter.UnableToComply), nil
	}
}

func (a *Adapter) handleUAR(req diameter.Message) (diameter.Message, error) {
	imsi, err := a.imsiFromRequest(req)
	if err != nil {
		return a.answerErr(req, diameter.UnableToComply), nil
	}
	ctx, cancel := a.ctx()
	exists, err := a.Core.Subscriber().Exists(ctx, imsi)
	cancel()
	if err != nil {
		return a.answerErr(req, diameter.UnableToComply), nil
	}
	if !exists {
		return a.answerErr(req, diameter.AuthenticationRejected), nil
	}
	uilog.Event(uilog.Magenta, "🔐", "REGISTER", "imsi %s  UAR ✓ S-CSCF assigned", imsi)
	ans := a.answerBase(req)
	ans.AVPs = append(ans.AVPs,
		diameter.ResultCode(diameter.Success),
		diameter.Str(diameter.AVPServerName, diameter.Vendor3GPP, true, a.SCSCFName),
	)
	return ans, nil
}

func (a *Adapter) handleLIR(req diameter.Message) (diameter.Message, error) {
	uilog.Event(uilog.Blue, "🧭", "LOCATE", "LIR ✓ terminating S-CSCF returned")
	ans := a.answerBase(req)
	ans.AVPs = append(ans.AVPs,
		diameter.ResultCode(diameter.Success),
		diameter.Str(diameter.AVPServerName, diameter.Vendor3GPP, true, a.SCSCFName),
	)
	return ans, nil
}

func (a *Adapter) handleSAR(req diameter.Message) (diameter.Message, error) {
	imsi, err := a.imsiFromRequest(req)
	if err != nil {
		return a.answerErr(req, diameter.UnableToComply), nil
	}
	ctx, cancel := a.ctx()
	msisdn, err := a.Core.Subscriber().MSISDN(ctx, imsi)
	cancel()
	if err != nil {
		return a.answerErr(req, diameter.UnableToComply), nil
	}
	xmlDoc, err := imssub.Build(imssub.Subscriber{IMSI: imsi, MSISDN: msisdn, PLMN: a.PLMN})
	if err != nil {
		return a.answerErr(req, diameter.UnableToComply), nil
	}
	uilog.Event(uilog.Magenta, "📇", "REGISTER", "imsi %s  SAR ✓ profile+iFC stored  · msisdn %s", imsi, msisdn)
	impi, _ := a.PLMN.IMPI(imsi)
	ans := a.answerBase(req)
	ans.AVPs = append(ans.AVPs,
		diameter.ResultCode(diameter.Success),
		diameter.Str(diameter.AVPUserName, 0, true, impi),
		diameter.Octet(diameter.AVPUserData, diameter.Vendor3GPP, true, xmlDoc),
	)
	return ans, nil
}

// handleMAR returns one AKAv1-MD5 vector. SIP-Authenticate carries RAND||AUTN;
// the S-CSCF builds its 401 challenge from it.
func (a *Adapter) handleMAR(req diameter.Message) (diameter.Message, error) {
	imsi, err := a.imsiFromRequest(req)
	if err != nil {
		return a.answerErr(req, diameter.UnableToComply), nil
	}
	ctx, cancel := a.ctx()
	vecs, err := a.Core.Auth().Vectors(ctx, api.AuthChallenge{IMSI: imsi, Count: 1})
	cancel()
	if err != nil || len(vecs) == 0 {
		return a.answerErr(req, diameter.AuthenticationRejected), nil
	}
	q := vecs[0]
	uilog.Event(uilog.Magenta, "🔑", "AUTH", "imsi %s  MAR ✓ AKA vector issued (AKAv1-MD5)", imsi)
	impi, _ := a.PLMN.IMPI(imsi)
	sipAuthenticate := append(append([]byte{}, q.RAND...), q.AUTN...) // RAND(16)||AUTN(16)

	item := diameter.Grouped(diameter.AVPSIPAuthDataItem, diameter.Vendor3GPP, true,
		diameter.U32(613, diameter.Vendor3GPP, true, 1), // SIP-Item-Number
		diameter.Str(diameter.AVPSIPAuthScheme, diameter.Vendor3GPP, true, "Digest-AKAv1-MD5"),
		diameter.Octet(diameter.AVPSIPAuthenticate, diameter.Vendor3GPP, true, sipAuthenticate),
		diameter.Octet(diameter.AVPSIPAuthorization, diameter.Vendor3GPP, true, q.XRES),
		diameter.Octet(diameter.AVPConfidentialityKey, diameter.Vendor3GPP, true, q.CK),
		diameter.Octet(diameter.AVPIntegrityKey, diameter.Vendor3GPP, true, q.IK),
	)
	ans := a.answerBase(req)
	ans.AVPs = append(ans.AVPs,
		diameter.ResultCode(diameter.Success),
		diameter.Str(diameter.AVPUserName, 0, true, impi),
	)
	if pi, e := diameter.Find(req.AVPs, diameter.AVPPublicIdentity, diameter.Vendor3GPP); e == nil {
		ans.AVPs = append(ans.AVPs, diameter.Str(diameter.AVPPublicIdentity, diameter.Vendor3GPP, true, pi.StrVal()))
	}
	ans.AVPs = append(ans.AVPs,
		diameter.U32(diameter.AVPSIPNumberAuthItems, diameter.Vendor3GPP, true, 1),
		item,
	)
	return ans, nil
}

// ---- request parsing / answers ----

func (a *Adapter) imsiFromRequest(req diameter.Message) (string, error) {
	if un, err := diameter.Find(req.AVPs, diameter.AVPUserName, 0); err == nil {
		if imsi := beforeAt(un.StrVal()); isDigits(imsi) {
			return imsi, nil
		}
	}
	if pi, err := diameter.Find(req.AVPs, diameter.AVPPublicIdentity, diameter.Vendor3GPP); err == nil {
		user := sipUser(pi.StrVal())
		if isDigits(user) {
			return user, nil
		}
	}
	return "", fmt.Errorf("cx: no IMSI derivable from User-Name/Public-Identity")
}

func (a *Adapter) answerBase(req diameter.Message) diameter.Message {
	ans := req.Answer()
	avps := []diameter.AVP{}
	if sid, err := diameter.Find(req.AVPs, diameter.AVPSessionID, 0); err == nil {
		avps = append(avps, sid)
	}
	avps = append(avps,
		diameter.VendorSpecificApplicationID(diameter.AppCx),
		diameter.U32(diameter.AVPAuthSessionState, 0, true, diameter.NoStateMaintained),
		diameter.OriginHost(a.OriginHost),
		diameter.OriginRealm(a.OriginRealm),
	)
	ans.AVPs = avps
	return ans
}

func (a *Adapter) answerErr(req diameter.Message, rc uint32) diameter.Message {
	inc(a.mErr)
	ans := a.answerBase(req)
	ans.Flags |= diameter.CmdError
	ans.AVPs = append(ans.AVPs, diameter.ResultCode(rc))
	return ans
}

func (a *Adapter) ctx() (context.Context, context.CancelFunc) {
	t := a.Timeout
	if t == 0 {
		// Keep this below the peer's transaction timeout so a stalled UDR
		// produces a Cx error answer rather than a silent timeout.
		t = 4 * time.Second
	}
	return context.WithTimeout(context.Background(), t)
}

func beforeAt(s string) string {
	if i := strings.IndexByte(s, '@'); i >= 0 {
		return s[:i]
	}
	return s
}

func sipUser(uri string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(uri, "sip:"), "sips:")
	if i := strings.IndexByte(s, '@'); i >= 0 {
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

// Serve runs the Diameter Cx server until it fails.
func (a *Adapter) Serve() error {
	srv := &dpeer.Server{
		ID: dpeer.Identity{
			OriginHost:  a.OriginHost,
			OriginRealm: a.OriginRealm,
			HostIP:      net.ParseIP(a.HostIP),
			ProductName: "setu-cx",
			VendorID:    diameter.Vendor3GPP,
			AppIDs:      []uint32{diameter.AppCx},
		},
		Handler: a.Handle,
	}
	if err := srv.Listen(a.Listen); err != nil {
		return err
	}
	log.Printf("cx: Diameter Cx on %s -> core %q (S-CSCF %s)", srv.Addr(), a.Core.Name(), a.SCSCFName)
	return srv.Serve()
}
