// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package config loads the JSON configuration file. JSON keeps the build
// dependency-free.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

type PLMN struct {
	MCC string `json:"mcc"`
	MNC string `json:"mnc"`
}

type Rx struct {
	Listen      string `json:"listen"`      // ":3868"
	OriginHost  string `json:"originHost"`  // rxgw.<domain>
	OriginRealm string `json:"originRealm"` // <domain>
	HostIP      string `json:"hostIP"`
	Strict      bool   `json:"strict"` // answer real Rx error codes instead of 2001
	WALPath     string `json:"walPath"`
}

type AppServer struct {
	ServerName      string `json:"serverName"`
	Method          string `json:"method,omitempty"`
	RequestURI      string `json:"requestURI,omitempty"`
	SessionCase     *int   `json:"sessionCase,omitempty"`
	Priority        int    `json:"priority"`
	DefaultHandling int    `json:"defaultHandling"`
}
type Cx struct {
	Listen      string      `json:"listen"` // ":3869"
	OriginHost  string      `json:"originHost"`
	OriginRealm string      `json:"originRealm"`
	HostIP      string      `json:"hostIP"`
	SCSCF       string      `json:"scscf"` // sip:<node>:6060
	Admin       string      `json:"admin"` // ":9102" metrics/healthz
	AppServers  []AppServer `json:"appServers"`
}

type SMS struct {
	Listen  string `json:"listen"` // "127.0.0.1:8090"
	SCSCF   string `json:"scscf"`  // "<node>:6060"
	SelfIP  string `json:"selfIP"` // advertised Via IP
	ViaPort int    `json:"viaPort"`
	Domain  string `json:"domain"`
}

type SDCore struct {
	PCF         string `json:"pcf"` // REQUIRED, e.g. https://192.0.2.20:29507
	UDR         string `json:"udr"` // REQUIRED, e.g. https://192.0.2.30:29504
	Insecure    bool   `json:"insecure"`
	NotifURI    string `json:"notifURI"`    // advertised to the PCF
	NotifListen string `json:"notifListen"` // ":7777"
	DialectFile string `json:"dialectFile"` // optional override of built-in dialect
}

type Config struct {
	PLMN   PLMN   `json:"plmn"`
	Domain string `json:"domain"`
	Core   string `json:"core"` // connector selector; "sdcore" today
	SDCore SDCore `json:"sdcore"`
	Rx     Rx     `json:"rx"`
	Cx     Cx     `json:"cx"`
	SMS    SMS    `json:"sms"`
}

// validateAppServers checks the cx.appServers rules when setu.json is loaded. A wrong rule would reach every user's
// profile, where the S-CSCF could refuse it (and nobody signs in) or it would never match; checked at start, setu
// stops with a clear message instead. Every problem is reported at once.
func validateAppServers(list []AppServer) error {
	var errs []error
	seen := map[int]int{} // priority -> the position of the rule that has it
	for i, a := range list {
		where := fmt.Sprintf("cx.appServers[%d] (%s)", i, a.ServerName)
		if !strings.HasPrefix(a.ServerName, "sip:") && !strings.HasPrefix(a.ServerName, "sips:") {
			errs = append(errs, fmt.Errorf("%s: serverName must start with sip: or sips:", where))
		}
		if a.Method == "" && a.RequestURI == "" && a.SessionCase == nil {
			errs = append(errs, fmt.Errorf("%s: no condition (method, requestURI or sessionCase): a rule without one catches every request", where))
		}
		if a.SessionCase != nil && (*a.SessionCase < 0 || *a.SessionCase > 3) {
			errs = append(errs, fmt.Errorf("%s: sessionCase %d: must be 0, 1, 2 or 3", where, *a.SessionCase))
		}
		if a.DefaultHandling != 0 && a.DefaultHandling != 1 {
			errs = append(errs, fmt.Errorf("%s: defaultHandling %d: must be 0 (continue) or 1 (terminate)", where, a.DefaultHandling))
		}
		if a.Priority < 0 {
			errs = append(errs, fmt.Errorf("%s: priority %d: must be 0 or more", where, a.Priority))
		}
		if j, ok := seen[a.Priority]; ok {
			errs = append(errs, fmt.Errorf("%s: priority %d is also cx.appServers[%d]'s: the S-CSCF needs one order", where, a.Priority, j))
		} else {
			seen[a.Priority] = i
		}
		if a.RequestURI != "" {
			if _, err := regexp.Compile(a.RequestURI); err != nil {
				errs = append(errs, fmt.Errorf("%s: requestURI %q is not a valid pattern: %v", where, a.RequestURI, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Load reads the config and fills in defaults. Deployment addresses are never
// defaulted: they must be set explicitly.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if c.Core == "" {
		c.Core = "sdcore"
	}
	if c.Domain == "" && c.PLMN.MCC != "" {
		mnc := c.PLMN.MNC
		if len(mnc) == 2 {
			mnc = "0" + mnc
		}
		c.Domain = fmt.Sprintf("ims.mnc%s.mcc%s.3gppnetwork.org", mnc, c.PLMN.MCC)
	}
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	def(&c.Rx.Listen, ":3868")
	def(&c.Rx.OriginHost, "rxgw."+c.Domain)
	def(&c.Rx.OriginRealm, c.Domain)
	def(&c.Cx.Listen, ":3869")
	def(&c.Cx.OriginHost, "hss."+c.Domain)
	def(&c.Cx.OriginRealm, c.Domain)
	def(&c.Cx.Admin, ":9102")
	def(&c.SMS.Listen, "127.0.0.1:8090")
	def(&c.SMS.Domain, c.Domain)
	if c.SMS.ViaPort == 0 {
		c.SMS.ViaPort = 8091
	}
	def(&c.SDCore.NotifListen, ":7777")
	if err := validateAppServers(c.Cx.AppServers); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}
