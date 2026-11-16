// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package smsf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coranlabs/SETU/chassis/sbi"
)

const defaultHeartbeat = 10 * time.Second

// nrfClient keeps the SMSF registered with the NRF (TS 29.510 Nnrf_NFManagement), so the AMF can find it for
// target-nf-type=SMSF.
type nrfClient struct {
	base, id string
	profile  []byte
	hc       *http.Client
	hb       time.Duration // heartbeat; 0 = what the NRF asks for

	started atomic.Bool
	once    sync.Once
	quit    chan struct{}
	done    chan struct{}
}

type nfServiceVersion struct {
	APIVersionInURI string `json:"apiVersionInUri"`
	APIFullVersion  string `json:"apiFullVersion"`
}

type profileService struct {
	ServiceInstanceID string             `json:"serviceInstanceId"`
	ServiceName       string             `json:"serviceName"`
	Versions          []nfServiceVersion `json:"versions"`
	Scheme            string             `json:"scheme"`
	NFServiceStatus   string             `json:"nfServiceStatus"`
	APIPrefix         string             `json:"apiPrefix,omitempty"`
	FQDN              string             `json:"fqdn,omitempty"`
	IPEndPoints       []ipEndPoint       `json:"ipEndPoints,omitempty"`
}

type registerProfile struct {
	NFInstanceID   string           `json:"nfInstanceId"`
	NFType         string           `json:"nfType"`
	NFStatus       string           `json:"nfStatus"`
	HeartBeatTimer int              `json:"heartBeatTimer"`
	PLMNList       []PLMN           `json:"plmnList,omitempty"`
	IPv4Addresses  []string         `json:"ipv4Addresses,omitempty"`
	FQDN           string           `json:"fqdn,omitempty"`
	NFServices     []profileService `json:"nfServices"`
}

func newNRFClient(cfg Config, hc *http.Client) (*nrfClient, error) {
	u, err := url.Parse(cfg.APIRoot)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("smsf: apiRoot %q is not scheme://host[:port]", cfg.APIRoot)
	}
	host := u.Hostname()
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	svc := profileService{
		ServiceInstanceID: "nsmsf-sms", ServiceName: "nsmsf-sms",
		// apiFullVersion is mandatory in NFServiceVersion; 2.0.0 is the first v2 of TS 29.540
		Versions: []nfServiceVersion{{APIVersionInURI: "v2", APIFullVersion: "2.0.0"}},
		Scheme:   u.Scheme, NFServiceStatus: "REGISTERED", APIPrefix: cfg.APIRoot,
	}
	p := registerProfile{NFInstanceID: cfg.NFInstanceID, NFType: "SMSF", NFStatus: "REGISTERED",
		HeartBeatTimer: int(defaultHeartbeat / time.Second), NFServices: []profileService{svc}}
	for _, pl := range cfg.PLMN {
		if pl.MCC != "" {
			p.PLMNList = append(p.PLMNList, pl)
		}
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		p.IPv4Addresses = []string{host}
		p.NFServices[0].IPEndPoints = []ipEndPoint{{IPv4Address: host, Transport: "TCP", Port: port}}
	} else {
		p.FQDN = host
		p.NFServices[0].FQDN = host
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return &nrfClient{base: cfg.NRF, id: cfg.NFInstanceID, profile: b, hc: hc, quit: make(chan struct{}), done: make(chan struct{})}, nil
}

func (n *nrfClient) instanceURL() string {
	return n.base + "/nnrf-nfm/v1/nf-instances/" + url.PathEscape(n.id)
}

// start registers, then heartbeats until close. Registration is retried: setu may start before the NRF.
func (n *nrfClient) start() {
	if n.started.Swap(true) {
		return
	}
	go func() {
		defer close(n.done)
		every := n.hb
		if every <= 0 {
			every = defaultHeartbeat
		}
		registered := false
		next := time.Duration(0)
		for {
			select {
			case <-n.quit:
				return
			case <-time.After(next):
			}
			if !registered {
				hb, err := n.register()
				if err != nil {
					log.Printf("smsf: NRF registration: %v", err)
				} else {
					registered = true
					if n.hb <= 0 && hb > 0 {
						every = hb
					}
					log.Printf("smsf: registered with the NRF as %s, heartbeat %v", n.id, every)
				}
			} else if st, err := n.heartbeat(); err != nil {
				log.Printf("smsf: NRF heartbeat: %v", err)
			} else if st == http.StatusNotFound {
				// the NRF lost the profile (it restarted, or let it expire): register again now
				registered = false
				log.Printf("smsf: the NRF no longer has this SMSF: registering again")
				next = 0
				continue
			}
			next = every
		}
	}()
}

// register PUTs the profile and returns the heartbeat the NRF asks for.
func (n *nrfClient) register() (time.Duration, error) {
	resp, body, err := n.do(context.Background(), http.MethodPut, "application/json", n.profile)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	var p struct {
		HeartBeatTimer int `json:"heartBeatTimer"`
	}
	_ = json.Unmarshal(body, &p)
	return time.Duration(p.HeartBeatTimer) * time.Second, nil
}

func (n *nrfClient) heartbeat() (int, error) {
	patch := []byte(`[{"op":"replace","path":"/nfStatus","value":"REGISTERED"}]`)
	resp, _, err := n.do(context.Background(), http.MethodPatch, "application/json-patch+json", patch)
	if err != nil {
		return 0, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
}

func (n *nrfClient) do(ctx context.Context, method, ctype string, body []byte) (*http.Response, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.instanceURL(), rd)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", ctype)
	}
	if err := (&sbi.Client{}).Decorate(req, sbi.APIRoot(n.base)); err != nil {
		return nil, nil, err
	}
	resp, err := n.hc.Do(req)
	if err != nil {
		return nil, nil, plainErr(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, b, nil
}

// close stops the heartbeat and deregisters, so the AMF stops choosing an SMSF that is gone.
func (n *nrfClient) close() {
	n.once.Do(func() {
		close(n.quit)
		if n.started.Load() {
			select {
			case <-n.done:
			case <-time.After(6 * time.Second): // a request in flight times out at 5 s
			}
		}
		resp, _, err := n.do(context.Background(), http.MethodDelete, "", nil)
		switch {
		case err != nil:
			log.Printf("smsf: NRF deregistration: %v", err)
		case resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound:
			log.Printf("smsf: NRF deregistration: status %d", resp.StatusCode)
		default:
			log.Printf("smsf: deregistered from the NRF")
		}
	})
}
