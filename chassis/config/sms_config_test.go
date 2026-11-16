// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

// the live setu.json has only listen, scscf and selfIP under sms and no smsf: every new key gets the value that keeps
// today's behaviour
func TestLoadSMSDefaults(t *testing.T) {
	c, err := Load(writeConfig(t, `{"plmn": {"mcc": "001", "mnc": "01"}, "sms": {"listen": "127.0.0.1:8090", "scscf": "192.0.2.1:6060", "selfIP": "192.0.2.2"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := c.SMS; s.SMSCAddress != "7" || s.MTOrder != "nas-first" || s.DeadlineSeconds != 25 || s.IMSAnswerSeconds != 4 || s.ViaPort != 8091 {
		t.Fatalf("sms = %+v", s)
	}
	if f := c.SMSF; f.Listen != ":29540" || f.ContextMaxAgeHours != 24 || f.TC1Seconds != 6 || f.MaxRetrans != 2 ||
		f.PagingWaitSeconds != 12 || f.APIRoot != "" || f.NRF != "" || f.AMF != "" || f.StateFile != "" || f.Insecure {
		t.Fatalf("smsf = %+v", f)
	}
	// a setu that does not run the smsf app needs none of it; one that does is told what is missing
	err = c.CheckSMSFApp()
	if err == nil || !strings.Contains(err.Error(), "smsf.apiRoot is required") || !strings.Contains(err.Error(), "smsf.amf is required") ||
		!strings.Contains(err.Error(), "smsf.trusted is required") {
		t.Fatalf("CheckSMSFApp = %v", err)
	}
}

func TestLoadSMSFKeys(t *testing.T) {
	c, err := Load(writeConfig(t, `{"sms": {"smscAddress": "+919900001234", "mtOrder": "ims-first", "deadlineSeconds": 30, "imsAnswerSeconds": 5},
		"smsf": {"listen": ":9540", "apiRoot": "http://192.0.2.10:9540", "nfInstanceId": "5b9f0c2e-7a1d-4e3f-9c8b-1a2b3c4d5e6f",
		"nrf": "http://nrf:29510", "amf": "https://amf:29518", "insecure": true, "stateFile": "/var/lib/setu/smsf.json",
		"contextMaxAgeHours": 48, "tc1Seconds": 8, "maxRetrans": 3, "pagingWaitSeconds": 10,
		"trusted": ["10.42.0.0/16", "192.0.2.10"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := c.SMS; s.SMSCAddress != "+919900001234" || s.MTOrder != "ims-first" || s.DeadlineSeconds != 30 || s.IMSAnswerSeconds != 5 {
		t.Fatalf("sms = %+v", s)
	}
	want := SMSF{Listen: ":9540", APIRoot: "http://192.0.2.10:9540", NFInstanceID: "5b9f0c2e-7a1d-4e3f-9c8b-1a2b3c4d5e6f",
		NRF: "http://nrf:29510", AMF: "https://amf:29518", Insecure: true, StateFile: "/var/lib/setu/smsf.json",
		ContextMaxAgeHours: 48, TC1Seconds: 8, MaxRetrans: 3, PagingWaitSeconds: 10,
		Trusted: []string{"10.42.0.0/16", "192.0.2.10"}}
	if !reflect.DeepEqual(c.SMSF, want) {
		t.Fatalf("smsf = %+v", c.SMSF)
	}
	if err := c.CheckSMSFApp(); err != nil {
		t.Fatalf("CheckSMSFApp = %v", err)
	}
	// with an NRF the AMF is found there; smsf.amf is only the fallback
	c.SMSF.AMF = ""
	if err := c.CheckSMSFApp(); err != nil {
		t.Fatalf("CheckSMSFApp with nrf, no amf = %v", err)
	}
}

func TestLoadSMSValidation(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"order", `{"sms": {"mtOrder": "nas-only"}}`, `sms.mtOrder "nas-only": must be nas-first or ims-first`},
		{"smsc letters", `{"sms": {"smscAddress": "smsc"}}`, `sms.smscAddress "smsc"`},
		{"negative deadline", `{"sms": {"deadlineSeconds": -1}}`, "sms.deadlineSeconds -1: must be more than 0"},
		{"answer not inside the deadline", `{"sms": {"deadlineSeconds": 4, "imsAnswerSeconds": 4}}`, "must be less than sms.deadlineSeconds 4"},
		{"retransmissions", `{"smsf": {"maxRetrans": 4}}`, "smsf.maxRetrans 4: must be 1, 2 or 3"},
		{"negative TC1*", `{"smsf": {"tc1Seconds": -6}}`, "smsf.tc1Seconds -6"},
		{"apiRoot not a URL", `{"smsf": {"apiRoot": "192.0.2.10:29540"}}`, `smsf.apiRoot "192.0.2.10:29540": must be http(s)://host[:port]`},
		{"nrf without an id", `{"smsf": {"nrf": "http://nrf:29510"}}`, `smsf.nfInstanceId "": a UUID is required with smsf.nrf`},
		{"nrf with a bad id", `{"smsf": {"nrf": "http://nrf:29510", "nfInstanceId": "smsf-1"}}`, `smsf.nfInstanceId "smsf-1"`},
	} {
		_, err := Load(writeConfig(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	// several mistakes, one error with all of them, next to an app-server mistake
	_, err := Load(writeConfig(t, `{"sms": {"mtOrder": "x"}, "smsf": {"maxRetrans": 9, "amf": "amf"},
		"cx": {"appServers": [{"serverName": "mcx", "method": "REGISTER"}]}}`))
	for _, want := range []string{"sms.mtOrder", "smsf.maxRetrans 9", "smsf.amf", "serverName"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from %v", want, err)
		}
	}
}

func TestSMSFTrustedPrefixes(t *testing.T) {
	got, err := SMSF{Trusted: []string{"10.42.7.9/16", "192.0.2.10", "2001:db8::1"}}.TrustedPrefixes()
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16"), netip.MustParsePrefix("192.0.2.10/32"),
		netip.MustParsePrefix("2001:db8::1/128")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	c := Config{SMSF: SMSF{APIRoot: "http://192.0.2.10:29540", AMF: "https://amf:29518", Trusted: []string{"10.42.0.0/16", "amf-pod"}}}
	if err := c.CheckSMSFApp(); err == nil || !strings.Contains(err.Error(), `smsf.trusted "amf-pod"`) {
		t.Fatalf("CheckSMSFApp = %v, want the bad entry named", err)
	}
}
