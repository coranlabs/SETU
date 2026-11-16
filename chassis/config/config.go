// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package config loads the JSON configuration file. JSON keeps the build
// dependency-free.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
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
	Listen           string `json:"listen"`  // "127.0.0.1:8090"
	SCSCF            string `json:"scscf"`   // "<node>:6060"
	SelfIP           string `json:"selfIP"`  // advertised Via IP
	ViaPort          int    `json:"viaPort"` // 8091 (proven)
	Domain           string `json:"domain"`
	SMSCAddress      string `json:"smscAddress"`      // RP-OA of every MT message; "7" (proven)
	MTOrder          string `json:"mtOrder"`          // "nas-first" (default) or "ims-first"
	DeadlineSeconds  int    `json:"deadlineSeconds"`  // a whole delivery, both domains: 25
	IMSAnswerSeconds int    `json:"imsAnswerSeconds"` // the wait for the S-CSCF's answer: 4
}

// SMSF is setu's SMSF (the smsf app): SMS over NAS for phones that send SMS on the 5G control channel.
type SMSF struct {
	Listen             string `json:"listen"`             // ":29540"
	APIRoot            string `json:"apiRoot"`            // how the AMF reaches it, e.g. http://<node>:29540; required
	NFInstanceID       string `json:"nfInstanceId"`       // a UUID; required with nrf
	NRF                string `json:"nrf"`                // register there and find each UE's AMF; "" = neither
	AMF                string `json:"amf"`                // the AMF's apiRoot; required without nrf
	Insecure           bool   `json:"insecure"`           // self-signed SBI certs (lab)
	StateFile          string `json:"stateFile"`          // "" = SMS contexts in memory only
	ContextMaxAgeHours int    `json:"contextMaxAgeHours"` // a saved context older than this is dropped at start: 24
	TC1Seconds         int    `json:"tc1Seconds"`         // the CP retransmission timer TC1*: 6
	MaxRetrans         int    `json:"maxRetrans"`         // CP-DATA retransmissions, 1-3 (TS 24.011): 2
	PagingWaitSeconds  int    `json:"pagingWaitSeconds"`  // the wait while the AMF pages the phone: 12
	// Trusted lists who may call the SMSF: CIDRs or single addresses (the AMF's pod network, its node). Required.
	Trusted []string `json:"trusted"`
}

// TrustedPrefixes is smsf.trusted parsed: "10.42.0.0/16" as is, a bare address as a /32 (or /128).
func (s SMSF) TrustedPrefixes() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, t := range s.Trusted {
		if p, err := netip.ParsePrefix(t); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(t)
		if err != nil {
			return nil, fmt.Errorf("smsf.trusted %q: not a CIDR or an address", t)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
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
	SMSF   SMSF   `json:"smsf"`
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

// validateSMS checks the sms and smsf keys every setu needs right. Every problem is reported at once.
func validateSMS(c *Config) error {
	var errs []error
	if m := c.SMS.MTOrder; m != "nas-first" && m != "ims-first" {
		errs = append(errs, fmt.Errorf("sms.mtOrder %q: must be nas-first or ims-first", m))
	}
	if a := strings.TrimPrefix(c.SMS.SMSCAddress, "+"); a == "" || strings.Trim(a, "0123456789") != "" || len(a) > 20 {
		errs = append(errs, fmt.Errorf("sms.smscAddress %q: must be up to 20 digits, + allowed in front", c.SMS.SMSCAddress))
	}
	for _, n := range []struct {
		key string
		v   int
	}{
		{"sms.deadlineSeconds", c.SMS.DeadlineSeconds}, {"sms.imsAnswerSeconds", c.SMS.IMSAnswerSeconds},
		{"smsf.contextMaxAgeHours", c.SMSF.ContextMaxAgeHours}, {"smsf.tc1Seconds", c.SMSF.TC1Seconds},
		{"smsf.pagingWaitSeconds", c.SMSF.PagingWaitSeconds},
	} {
		if n.v <= 0 {
			errs = append(errs, fmt.Errorf("%s %d: must be more than 0", n.key, n.v))
		}
	}
	if r := c.SMSF.MaxRetrans; r < 1 || r > 3 {
		errs = append(errs, fmt.Errorf("smsf.maxRetrans %d: must be 1, 2 or 3 (TS 24.011)", r))
	}
	if c.SMS.IMSAnswerSeconds >= c.SMS.DeadlineSeconds && c.SMS.DeadlineSeconds > 0 {
		errs = append(errs, fmt.Errorf("sms.imsAnswerSeconds %d: must be less than sms.deadlineSeconds %d", c.SMS.IMSAnswerSeconds, c.SMS.DeadlineSeconds))
	}
	for _, u := range []struct{ key, v string }{{"smsf.apiRoot", c.SMSF.APIRoot}, {"smsf.nrf", c.SMSF.NRF}, {"smsf.amf", c.SMSF.AMF}} {
		if u.v == "" {
			continue
		}
		if p, err := url.Parse(u.v); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			errs = append(errs, fmt.Errorf("%s %q: must be http(s)://host[:port]", u.key, u.v))
		}
	}
	if c.SMSF.NRF != "" && !uuidRE.MatchString(c.SMSF.NFInstanceID) {
		errs = append(errs, fmt.Errorf("smsf.nfInstanceId %q: a UUID is required with smsf.nrf", c.SMSF.NFInstanceID))
	}
	return errors.Join(errs...)
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// CheckSMSFApp checks what the smsf app needs beyond validateSMS: where the AMF reaches it, and where it reaches the
// AMF. Only a setu running the smsf app needs these, so Load does not ask for them.
func (c *Config) CheckSMSFApp() error {
	var errs []error
	if c.SMSF.APIRoot == "" {
		errs = append(errs, errors.New("smsf.apiRoot is required: the AMF and its failure callbacks reach the SMSF there"))
	}
	if c.SMSF.NRF == "" && c.SMSF.AMF == "" {
		errs = append(errs, errors.New("smsf.amf is required without smsf.nrf"))
	}
	if len(c.SMSF.Trusted) == 0 {
		errs = append(errs, errors.New("smsf.trusted is required: the addresses the AMF calls from (anyone else could send sms as any subscriber)"))
	} else if _, err := c.SMSF.TrustedPrefixes(); err != nil {
		errs = append(errs, err)
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
	def(&c.SMS.SMSCAddress, "7")
	def(&c.SMS.MTOrder, "nas-first")
	defInt := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	defInt(&c.SMS.DeadlineSeconds, 25)
	defInt(&c.SMS.IMSAnswerSeconds, 4)
	def(&c.SMSF.Listen, ":29540")
	defInt(&c.SMSF.ContextMaxAgeHours, 24)
	defInt(&c.SMSF.TC1Seconds, 6)
	defInt(&c.SMSF.MaxRetrans, 2)
	defInt(&c.SMSF.PagingWaitSeconds, 12)
	def(&c.SDCore.NotifListen, ":7777")
	if err := errors.Join(validateAppServers(c.Cx.AppServers), validateSMS(&c)); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}
