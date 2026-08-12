// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "setu.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A minimal config must come back usable: the home domain is derived from the
// PLMN and every listener gets its standard port.
func TestLoadDefaults(t *testing.T) {
	c, err := Load(write(t, `{"plmn":{"mcc":"001","mnc":"01"},
		"sdcore":{"pcf":"https://pcf:29507","udr":"https://udr:29504"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"domain", c.Domain, "ims.mnc001.mcc001.3gppnetwork.org"},
		{"core", c.Core, "sdcore"},
		{"rx.listen", c.Rx.Listen, ":3868"},
		{"rx.originHost", c.Rx.OriginHost, "rxgw.ims.mnc001.mcc001.3gppnetwork.org"},
		{"rx.originRealm", c.Rx.OriginRealm, "ims.mnc001.mcc001.3gppnetwork.org"},
		{"cx.listen", c.Cx.Listen, ":3869"},
		{"cx.originHost", c.Cx.OriginHost, "hss.ims.mnc001.mcc001.3gppnetwork.org"},
		{"cx.admin", c.Cx.Admin, ":9102"},
		{"sms.listen", c.SMS.Listen, "127.0.0.1:8090"},
		{"sms.domain", c.SMS.Domain, "ims.mnc001.mcc001.3gppnetwork.org"},
		{"notifListen", c.SDCore.NotifListen, ":7777"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if c.SMS.ViaPort != 8091 {
		t.Errorf("sms.viaPort = %d, want 8091", c.SMS.ViaPort)
	}
}

// A two-digit MNC is zero-padded to three in the home domain (TS 23.003).
func TestDomainMNCPadding(t *testing.T) {
	for _, tc := range []struct{ mcc, mnc, want string }{
		{"001", "01", "ims.mnc001.mcc001.3gppnetwork.org"},
		{"310", "260", "ims.mnc260.mcc310.3gppnetwork.org"},
	} {
		c, err := Load(write(t, `{"plmn":{"mcc":"`+tc.mcc+`","mnc":"`+tc.mnc+`"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if c.Domain != tc.want {
			t.Errorf("mcc=%s mnc=%s -> %q, want %q", tc.mcc, tc.mnc, c.Domain, tc.want)
		}
	}
}

// Explicit values must survive; defaults only fill blanks.
func TestExplicitValuesWin(t *testing.T) {
	c, err := Load(write(t, `{"plmn":{"mcc":"001","mnc":"01"},"domain":"ims.example.net",
		"rx":{"listen":":4868","strict":true},"sms":{"viaPort":9000}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Domain != "ims.example.net" {
		t.Errorf("domain = %q", c.Domain)
	}
	if c.Rx.Listen != ":4868" || !c.Rx.Strict {
		t.Errorf("rx = %+v", c.Rx)
	}
	if c.SMS.ViaPort != 9000 {
		t.Errorf("viaPort = %d", c.SMS.ViaPort)
	}
	if c.Rx.OriginHost != "rxgw.ims.example.net" {
		t.Errorf("originHost = %q, want it derived from the explicit domain", c.Rx.OriginHost)
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file should error")
	}
	if _, err := Load(write(t, `{"plmn":`)); err == nil {
		t.Error("malformed JSON should error")
	}
}
