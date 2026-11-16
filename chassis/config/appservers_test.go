// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func intp(v int) *int { return &v }

// the two TossiCore MCX rules, as setu.json carries them
func mcxRules() []AppServer {
	return []AppServer{
		{ServerName: "sip:192.0.2.20:5070", Method: "REGISTER", SessionCase: intp(0), Priority: 10, DefaultHandling: 0},
		{ServerName: "sip:192.0.2.20:5070", RequestURI: "^sip:(mcptt|mcdata|mcvideo)@", SessionCase: intp(0), Priority: 20, DefaultHandling: 1},
	}
}

func TestValidateAppServersAcceptsTheMCXRulesAndNone(t *testing.T) {
	if err := validateAppServers(mcxRules()); err != nil {
		t.Fatalf("the MCX rules refused: %v", err)
	}
	if err := validateAppServers(nil); err != nil {
		t.Fatalf("no rules refused: %v", err)
	}
}

// each wrong rule is refused with a message naming its position and what is wrong
func TestValidateAppServersNamesEachProblem(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(a *AppServer)
		want   string
	}{
		{"not a SIP address", func(a *AppServer) { a.ServerName = "192.0.2.20:5070" }, "serverName must start with sip: or sips:"},
		{"sips is allowed", func(a *AppServer) { a.ServerName = "sips:mcx.example.org:5071" }, ""},
		{"no condition", func(a *AppServer) { a.Method, a.SessionCase = "", nil }, "no condition"},
		{"session case 7", func(a *AppServer) { a.SessionCase = intp(7) }, "sessionCase 7: must be 0, 1, 2 or 3"},
		{"session case -1", func(a *AppServer) { a.SessionCase = intp(-1) }, "sessionCase -1"},
		{"session case 3 is allowed", func(a *AppServer) { a.SessionCase = intp(3) }, ""},
		{"default handling 2", func(a *AppServer) { a.DefaultHandling = 2 }, "defaultHandling 2: must be 0 (continue) or 1 (terminate)"},
		{"default handling 1 is allowed", func(a *AppServer) { a.DefaultHandling = 1 }, ""},
		{"negative priority", func(a *AppServer) { a.Priority = -5 }, "priority -5: must be 0 or more"},
		// a clash is reported on the later rule, naming the earlier one
		{"same priority as rule 1", func(a *AppServer) { a.Priority = 20 }, "cx.appServers[1] (sip:192.0.2.20:5070): priority 20 is also cx.appServers[0]'s"},
		{"broken pattern", func(a *AppServer) { a.RequestURI = "^sip:(mcptt@" }, `requestURI "^sip:(mcptt@" is not a valid pattern`},
	} {
		rules := mcxRules()
		c.change(&rules[0])
		err := validateAppServers(rules)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && err == nil:
			t.Errorf("%s: accepted, want %q", c.name, c.want)
		case c.want != "" && !strings.Contains(err.Error(), "cx.appServers[0]"):
			t.Errorf("%s: %q does not name the rule", c.name, err)
		case c.want != "" && !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: %q, want %q in it", c.name, err, c.want)
		}
	}
}

// three mistakes in one rule give one error with all three
func TestValidateAppServersReportsAllAtOnce(t *testing.T) {
	rules := mcxRules()
	rules[0].ServerName, rules[0].SessionCase, rules[0].DefaultHandling = "mcx", intp(9), 4
	err := validateAppServers(rules)
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"serverName", "sessionCase 9", "defaultHandling 4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from %q", want, err)
		}
	}
}

// Load reads cx.appServers from setu.json and stops on a wrong rule
func TestLoadReadsAndChecksAppServers(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.json", `{"plmn": {"mcc": "001", "mnc": "01"}, "cx": {"appServers": [
		{"serverName": "sip:192.0.2.20:5070", "method": "REGISTER", "sessionCase": 0, "priority": 10, "defaultHandling": 0},
		{"serverName": "sip:192.0.2.20:5070", "requestURI": "^sip:(mcptt|mcdata|mcvideo)@", "sessionCase": 0, "priority": 20, "defaultHandling": 1}]}}`)
	c, err := Load(good)
	if err != nil {
		t.Fatalf("good file: %v", err)
	}
	as := c.Cx.AppServers
	if len(as) != 2 || as[0].Method != "REGISTER" || as[0].SessionCase == nil || *as[0].SessionCase != 0 ||
		as[1].RequestURI != "^sip:(mcptt|mcdata|mcvideo)@" || as[1].DefaultHandling != 1 {
		t.Fatalf("read as %+v", as)
	}

	bad := write("bad.json", `{"cx": {"appServers": [{"serverName": "sip:mcx:5070", "method": "REGISTER", "sessionCase": 7}]}}`)
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "sessionCase 7") {
		t.Fatalf("bad file: %v", err)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "setu.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
