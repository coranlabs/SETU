// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package sbi adds 5G SBI conformance to the bridges' HTTP calls: OAuth2 access
// tokens obtained from the NRF (Nnrf_AccessToken, TS 29.510 §5.4 / TS 33.501) and the
// mandatory 3gpp-Sbi-* headers (TS 29.500). Stdlib only.
package sbi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenSource fetches and caches an OAuth2 client-credentials access token from the
// NRF for a given target NF type + scope. Safe for concurrent use.
type TokenSource struct {
	NRFTokenURL  string // {nrfApiRoot}/oauth2/token
	NFInstanceID string // this NF's instance id (client)
	NFType       string // this NF's type, e.g. "AF"
	TargetNFType string // e.g. "PCF"
	Scope        string // e.g. "npcf-policyauthorization"
	HTTP         *http.Client
	Now          func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

type tokenResp struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

// Token returns a valid cached token, refreshing from the NRF when within 10s of
// expiry.
func (t *TokenSource) Token() (string, error) {
	now := t.now()
	t.mu.Lock()
	if t.token != "" && now.Before(t.expires.Add(-10*time.Second)) {
		tok := t.token
		t.mu.Unlock()
		return tok, nil
	}
	t.mu.Unlock()

	form := url.Values{
		"grant_type":   {"client_credentials"},
		"nfInstanceId": {t.NFInstanceID},
		"nfType":       {t.NFType},
		"targetNfType": {t.TargetNFType},
		"scope":        {t.Scope},
	}
	req, err := http.NewRequest(http.MethodPost, t.NRFTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("sbi: NRF token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sbi: NRF token status %d: %s", resp.StatusCode, string(body))
	}
	var tr tokenResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("sbi: bad token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("sbi: NRF returned empty access_token")
	}
	ttl := tr.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	t.mu.Lock()
	t.token = tr.AccessToken
	t.expires = now.Add(time.Duration(ttl) * time.Second)
	t.mu.Unlock()
	return tr.AccessToken, nil
}

func (t *TokenSource) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *TokenSource) httpClient() *http.Client {
	if t.HTTP != nil {
		return t.HTTP
	}
	return http.DefaultClient
}

// Client decorates outbound SBI requests with OAuth2 + 3gpp-Sbi-* headers.
type Client struct {
	Token *TokenSource // optional: if nil, no Authorization header is added
}

// Decorate adds the 3gpp-Sbi-* headers and (if a TokenSource is set) the OAuth2
// Bearer token. targetAPIRoot is the scheme://host[:port] of the target NF.
func (c *Client) Decorate(req *http.Request, targetAPIRoot string) error {
	req.Header.Set("3gpp-Sbi-Sender-Timestamp", nowUTC().Format(http.TimeFormat))
	if targetAPIRoot != "" {
		req.Header.Set("3gpp-Sbi-Target-apiRoot", targetAPIRoot)
	}
	if c != nil && c.Token != nil {
		tok, err := c.Token.Token()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return nil
}

// APIRoot returns the scheme://host[:port] of a full URL (the 3gpp-Sbi-Target-apiRoot).
func APIRoot(fullURL string) string {
	u, err := url.Parse(fullURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

var nowUTC = func() time.Time { return time.Now().UTC() }
