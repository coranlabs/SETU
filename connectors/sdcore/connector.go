// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package sdcore implements the CoreConnector for OMEC/Aether SD-Core:
// Npcf_PolicyAuthorization (N5) for media authorization, Nudr for subscriber data,
// and locally generated AKA vectors. Tunables live in dialects/sdcore.json.
package sdcore

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	api "github.com/coranlabs/SETU/api/v1"
	"github.com/coranlabs/SETU/connectors/sdcore/rx2n5"
	"github.com/coranlabs/SETU/connectors/sdcore/udr"
)

// Config carries no defaults for deployment addresses on purpose: a stale
// compiled-in PCF address fails silently after a core reinstall.
type Config struct {
	PCFBase     string        // required, e.g. https://pcf:29507
	UDRBase     string        // required, e.g. https://udr:29504
	PLMNID      string        // "<mcc><mnc>", scopes the am-data path
	Insecure    bool          // skip TLS verification (self-signed SBI certs)
	HTTPTimeout time.Duration // default 20s
	NotifURI    string        // AF notification endpoint given to the PCF
	NotifListen string        // local listen address for it; "" disables
	Dialect     rx2n5.Config
}

type Connector struct {
	cfg    Config
	hc     *http.Client
	events chan api.SessionEvent
}

func New(cfg Config) (*Connector, error) {
	if cfg.PCFBase == "" || cfg.UDRBase == "" {
		return nil, errors.New("sdcore: PCFBase and UDRBase are required (no compiled-in defaults, by design)")
	}
	if cfg.NotifURI == "" {
		return nil, errors.New("sdcore: NotifURI is required (the PCF mandates evSubsc.notifUri; a placeholder would lose notifications silently)")
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 20 * time.Second
	}
	if cfg.Dialect.DNN == "" {
		cfg.Dialect = rx2n5.DefaultConfig()
	}
	if cfg.NotifURI != "" {
		cfg.Dialect.NotifURI = cfg.NotifURI
	}
	hc := &http.Client{Timeout: cfg.HTTPTimeout}
	if cfg.Insecure {
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	c := &Connector{cfg: cfg, hc: hc, events: make(chan api.SessionEvent, 64)}
	if cfg.NotifListen != "" {
		go c.serveNotif()
	}
	return c, nil
}

func (c *Connector) Name() string { return "sdcore" }

func (c *Connector) Capabilities() api.Caps {
	return api.Caps{
		PolicyUpdate:  false, // this PCF has no N5 PATCH
		PolicyRead:    false,
		Notifications: true,
		AuthVector:    api.AuthLocalMilenage,
		SQNStep:       32,
	}
}

func (c *Connector) Policy() api.PolicyBackend         { return (*policy)(c) }
func (c *Connector) Subscriber() api.SubscriberBackend { return (*subscriber)(c) }
func (c *Connector) Auth() api.AuthBackend             { return (*auth)(c) }
func (c *Connector) Events() api.EventSource           { return (*eventSource)(c) }

// ---- PolicyBackend ----

type policy Connector

func (p *policy) Create(ctx context.Context, g *api.MediaGrant) (api.CoreRef, error) {
	asc, err := rx2n5.Translate(g.Raw, p.cfg.Dialect, resolveGPSI)
	if err != nil {
		return "", fmt.Errorf("%w: translate: %v", api.ErrRejected, err)
	}
	if asc.AscReqData.EvSubsc.NotifUri == "" {
		asc.AscReqData.EvSubsc.NotifUri = p.cfg.Dialect.NotifURI
	}
	body, _ := json.Marshal(asc)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.cfg.PCFBase+"/npcf-policyauthorization/v1/app-sessions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", api.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		err := fmt.Errorf("PCF app-session HTTP %d: %s", resp.StatusCode, string(rb))
		if resp.StatusCode == http.StatusNotFound || bytes.Contains(rb, []byte("Supi is not supported")) {
			return "", fmt.Errorf("%w: %v", api.ErrNoSession, err)
		}
		return "", fmt.Errorf("%w: %v", api.ErrRejected, err)
	}
	return api.CoreRef(resp.Header.Get("Location")), nil
}

func (p *policy) Update(ctx context.Context, ref api.CoreRef, g *api.MediaGrant) error {
	return api.ErrUnsupported
}

// Delete removes the app-session. The PCF returns a Location built from its own
// service name, which the bridge host usually cannot resolve, so the path is
// re-attached to PCFBase before use.
func (p *policy) Delete(ctx context.Context, ref api.CoreRef) error {
	loc := string(ref)
	delURL := loc + "/delete"
	if i := strings.Index(loc, "/npcf-policyauthorization"); i >= 0 {
		delURL = p.cfg.PCFBase + loc[i:] + "/delete"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, delURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", api.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil // already gone
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%w: delete HTTP %d %s", api.ErrRejected, resp.StatusCode, delURL)
	}
	return nil
}

// resolveGPSI turns a SIP/tel IMPU into the "msisdn-<number>" GPSI the PCF stores.
// Rejects 15-digit values: those are IMSIs, not MSISDNs.
func resolveGPSI(impu string) (string, error) {
	u := impu
	for _, p := range []string{"sip:", "sips:", "tel:"} {
		u = strings.TrimPrefix(u, p)
	}
	if i := strings.IndexByte(u, '@'); i >= 0 {
		u = u[:i]
	}
	if i := strings.IndexByte(u, ';'); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimPrefix(u, "+")
	if u == "" {
		return "", fmt.Errorf("resolveGPSI: empty user part in %q", impu)
	}
	for _, c := range u {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("resolveGPSI: non-numeric user part in %q", impu)
		}
	}
	if len(u) == 15 {
		return "", fmt.Errorf("resolveGPSI: %q looks like an IMSI, not an MSISDN", impu)
	}
	return "msisdn-" + u, nil
}

// ---- SubscriberBackend ----

type subscriber Connector

// Exists reports whether the subscriber has authentication data provisioned.
func (s *subscriber) Exists(ctx context.Context, imsi string) (bool, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, authSubURL(s.cfg.UDRBase, imsi), nil)
	resp, err := s.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", api.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK, nil
}

// MSISDN reads am-data. Note the PLMN segment in the path: without it the UDR
// returns 404 even for a provisioned subscriber.
func (s *subscriber) MSISDN(ctx context.Context, imsi string) (string, error) {
	url := fmt.Sprintf("%s/nudr-dr/v2/subscription-data/imsi-%s/%s/provisioned-data/am-data",
		strings.TrimRight(s.cfg.UDRBase, "/"), imsi, s.cfg.PLMNID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", api.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: UDR am-data status %d", api.ErrRejected, resp.StatusCode)
	}
	return udr.ParseMSISDN(body)
}

func authSubURL(base, imsi string) string {
	return fmt.Sprintf("%s/nudr-dr/v2/subscription-data/imsi-%s/authentication-data/authentication-subscription",
		strings.TrimRight(base, "/"), imsi)
}

// ---- EventSource ----

type eventSource Connector

func (e *eventSource) Events() <-chan api.SessionEvent { return e.events }

// serveNotif is the AF notification endpoint advertised to the PCF. It always
// answers 204 and republishes the body on the event channel.
func (c *Connector) serveNotif() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		select {
		case c.events <- api.SessionEvent{Kind: api.EvNotifyRaw, Detail: string(body)}:
		default: // drop rather than block the PCF
		}
		w.WriteHeader(http.StatusNoContent)
	})
	log.Printf("sdcore: AF notif endpoint on %s", c.cfg.NotifListen)
	srv := &http.Server{Addr: c.cfg.NotifListen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("sdcore: notif endpoint: %v", err)
	}
}
