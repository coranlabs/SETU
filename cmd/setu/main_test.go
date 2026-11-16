// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/coranlabs/SETU/chassis/config"
)

// every field of a cx.appServers rule reaches the rule imssub.Build writes
func TestAppServerTriggersKeepsEveryField(t *testing.T) {
	sc := 0
	got := appServerTriggers([]config.AppServer{{ServerName: "sip:mcx:5070", Method: "REGISTER", RequestURI: "^sip:mcptt@",
		SessionCase: &sc, Priority: 20, DefaultHandling: 1}})
	if len(got) != 1 {
		t.Fatalf("%d rules, want 1", len(got))
	}
	g := got[0]
	if g.ServerName != "sip:mcx:5070" || g.Method != "REGISTER" || g.RequestURI != "^sip:mcptt@" || g.SessionCase == nil ||
		*g.SessionCase != 0 || g.Priority != 20 || g.DefaultHandling != 1 {
		t.Fatalf("copied as %+v", g)
	}
	if n := len(appServerTriggers(nil)); n != 0 {
		t.Fatalf("no rules gave %d", n)
	}
}

// -apps accepts the smsf app next to the others and refuses an unknown one, naming the valid list
func TestParseApps(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{"rx,cx,sms", "rx cx sms"},
		{"rx,cx,sms,smsf", "rx cx sms smsf"},
		{" smsf ", "smsf"},
		{"sms,,smsf,", "sms smsf"},
	} {
		got, err := parseApps(c.in)
		if err != nil || strings.Join(got, " ") != c.want {
			t.Errorf("parseApps(%q) = %v, %v, want %s", c.in, got, err, c.want)
		}
	}
	if _, err := parseApps("rx,smsc"); err == nil || !strings.Contains(err.Error(), `unknown app "smsc" (valid: rx,cx,sms,smsf)`) {
		t.Fatalf("unknown app: %v", err)
	}
}
