// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package smsf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/coranlabs/SETU/chassis/sbi"
)

// amfClient sends N1N2MessageTransfer (TS 29.518 5.2.2.3.1) to the AMF serving a UE.
type amfClient struct {
	hc     *http.Client
	sbi    *sbi.Client
	nrf    string // "" = no lookup
	static string // smsf.amf

	mu    sync.Mutex
	cache map[string]string // AMF NF instance id -> its namf-comm apiRoot, from the NRF
}

type refToBinary struct {
	ContentID string `json:"contentId"`
}

type n1MessageContainer struct {
	N1MessageClass   string      `json:"n1MessageClass"`
	N1MessageContent refToBinary `json:"n1MessageContent"`
}

// n1n2Req is the JSON part of an N1N2MessageTransfer carrying SMS: an N1 container only, no PDU session, no N2.
type n1n2Req struct {
	N1MessageContainer     n1MessageContainer `json:"n1MessageContainer"`
	LastMsgIndication      bool               `json:"lastMsgIndication"`
	N1N2FailureTxfNotifURI string             `json:"n1n2FailureTxfNotifURI,omitempty"`
	NFID                   string             `json:"nfId,omitempty"`
}

// The part names TossiCore's multipart decoder binds by: it matches a part's Content-ID to the field's name.
const (
	jsonPartID = "jsonData"
	n1PartID   = "binaryDataN1Message"
)

// n1n2Body builds the multipart/related body carrying one CP message.
func n1n2Body(msg []byte, last bool, notif, nfID string) ([]byte, string, error) {
	js, err := json.Marshal(n1n2Req{
		N1MessageContainer:     n1MessageContainer{N1MessageClass: "SMS", N1MessageContent: refToBinary{ContentID: n1PartID}},
		LastMsgIndication:      last,
		N1N2FailureTxfNotifURI: notif,
		NFID:                   nfID,
	})
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range []struct {
		ctype, id string
		body      []byte
	}{{"application/json", jsonPartID, js}, {"application/vnd.3gpp.5gnas", n1PartID, msg}} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", p.ctype)
		h.Set("Content-ID", p.id)
		pw, err := mw.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := pw.Write(p.body); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	ct := mime.FormatMediaType("multipart/related", map[string]string{"boundary": mw.Boundary(), "type": "application/json"})
	return buf.Bytes(), ct, nil
}

// transfer sends msg to the UE through the AMF serving it and returns the AMF's HTTP status.
func (c *amfClient) transfer(ctx context.Context, amfID, supi string, msg []byte, last bool, notif, nfID string) (int, error) {
	root, err := c.root(ctx, amfID)
	if err != nil {
		return 0, err
	}
	body, ct, err := n1n2Body(msg, last, notif, nfID)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		root+"/namf-comm/v1/ue-contexts/"+url.PathEscape(supi)+"/n1-n2-messages", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Accept", "application/json, application/problem+json")
	if err := c.sbi.Decorate(req, sbi.APIRoot(root)); err != nil {
		return 0, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.forget(amfID) // the AMF may have moved: look it up again next time
		return 0, plainErr(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, nil
}

// plainErr drops the request URL from an HTTP client error: it names the subscriber, and the error is logged.
func plainErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

func (c *amfClient) forget(amfID string) {
	c.mu.Lock()
	delete(c.cache, amfID)
	c.mu.Unlock()
}

// root is the apiRoot of the AMF with this NF instance id: the NRF's answer, cached, or else smsf.amf.
func (c *amfClient) root(ctx context.Context, amfID string) (string, error) {
	if c.nrf != "" && amfID != "" {
		c.mu.Lock()
		r, ok := c.cache[amfID]
		c.mu.Unlock()
		if ok {
			return r, nil
		}
		r, err := c.discover(ctx, amfID)
		if err == nil {
			c.mu.Lock()
			c.cache[amfID] = r
			c.mu.Unlock()
			return r, nil
		}
		if c.static == "" {
			return "", err
		}
	}
	if c.static == "" {
		return "", errors.New("smsf: no AMF address (set smsf.amf or smsf.nrf)")
	}
	return c.static, nil
}

type ipEndPoint struct {
	IPv4Address string `json:"ipv4Address,omitempty"`
	Transport   string `json:"transport,omitempty"`
	Port        int    `json:"port,omitempty"`
}

type nfService struct {
	ServiceName string       `json:"serviceName"`
	Scheme      string       `json:"scheme"`
	APIPrefix   string       `json:"apiPrefix"`
	FQDN        string       `json:"fqdn"`
	IPEndPoints []ipEndPoint `json:"ipEndPoints"`
}

type nfProfile struct {
	NFInstanceID  string               `json:"nfInstanceId"`
	IPv4Addresses []string             `json:"ipv4Addresses"`
	FQDN          string               `json:"fqdn"`
	NFServices    []nfService          `json:"nfServices"`
	NFServiceList map[string]nfService `json:"nfServiceList"` // the Rel-16+ form of nfServices
}

// discover asks the NRF for the AMF with this NF instance id (TS 29.510 Nnrf_NFDiscovery).
func (c *amfClient) discover(ctx context.Context, amfID string) (string, error) {
	q := url.Values{"target-nf-type": {"AMF"}, "requester-nf-type": {"SMSF"}, "target-nf-instance-id": {amfID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.nrf+"/nnrf-disc/v1/nf-instances?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	if err := (&sbi.Client{}).Decorate(req, sbi.APIRoot(c.nrf)); err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("smsf: NRF discovery: %w", plainErr(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("smsf: NRF discovery of AMF %s: status %d", amfID, resp.StatusCode)
	}
	var res struct {
		NFInstances []nfProfile `json:"nfInstances"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("smsf: NRF discovery: %w", err)
	}
	for _, p := range res.NFInstances {
		if p.NFInstanceID == amfID || p.NFInstanceID == "" {
			if r := commRoot(p); r != "" {
				return r, nil
			}
		}
	}
	return "", fmt.Errorf("smsf: the NRF knows no namf-comm for AMF %s", amfID)
}

// commRoot builds the namf-comm apiRoot from an AMF profile.
func commRoot(p nfProfile) string {
	svcs := p.NFServices
	for _, s := range p.NFServiceList {
		svcs = append(svcs, s)
	}
	for _, s := range svcs {
		if s.ServiceName != "namf-comm" {
			continue
		}
		if s.APIPrefix != "" {
			return strings.TrimRight(s.APIPrefix, "/")
		}
		scheme := s.Scheme
		if scheme == "" {
			scheme = "http"
		}
		host, port := s.FQDN, 0
		if len(s.IPEndPoints) > 0 {
			if s.IPEndPoints[0].IPv4Address != "" {
				host = s.IPEndPoints[0].IPv4Address
			}
			port = s.IPEndPoints[0].Port
		}
		if host == "" && len(p.IPv4Addresses) > 0 {
			host = p.IPv4Addresses[0]
		}
		if host == "" {
			host = p.FQDN
		}
		if host == "" {
			return ""
		}
		if port == 0 {
			return scheme + "://" + host
		}
		return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port))
	}
	return ""
}
