// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Command setu bridges an IMS core (Diameter Rx/Cx and SMS over IP) to a 5G core.
// Run all protocol adapters in one process or split them with -apps.
//
//	setu -config /etc/setu/setu.json -apps rx,cx,sms
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/coranlabs/SETU/adapters/cx"
	"github.com/coranlabs/SETU/adapters/rx"
	"github.com/coranlabs/SETU/adapters/sms"
	"github.com/coranlabs/SETU/adapters/smsf"
	api "github.com/coranlabs/SETU/api/v1"
	"github.com/coranlabs/SETU/chassis/config"
	"github.com/coranlabs/SETU/connectors/sdcore"
	"github.com/coranlabs/SETU/connectors/sdcore/rx2n5"
	"github.com/coranlabs/SETU/fabric"
	"github.com/coranlabs/SETU/ims/ident"
	"github.com/coranlabs/SETU/ims/imssub"
	"github.com/coranlabs/SETU/sms/route"
)

const version = "0.1.0"

func main() {
	cfgPath := flag.String("config", "/etc/setu/setu.json", "path to setu.json")
	apps := flag.String("apps", "rx,cx,sms", "comma-separated apps to run: rx,cx,sms,smsf")
	flag.Parse()
	names, err := parseApps(*apps)
	if err != nil {
		log.Fatalf("setu: %v", err)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("setu: %v", err)
	}

	core, err := buildConnector(cfg)
	if err != nil {
		log.Fatalf("setu: connector: %v", err)
	}
	log.Printf("setu %s: core=%q caps=%+v", version, core.Name(), core.Capabilities())

	// core notifications; not yet turned into Rx RAR/ASR
	go func() {
		for ev := range core.Events().Events() {
			log.Printf("setu: core event kind=%s detail=%.200s", ev.Kind, ev.Detail)
		}
	}()

	errc := make(chan error, len(names)+1)
	run := func(name string, f func() error) {
		go func() {
			log.Printf("setu: starting %s", name)
			errc <- f()
		}()
	}
	var closers []func() error

	// One SMS router for both ways in (the S-CSCF's /mo and the SMSF's sendsms): a message reaches its recipient
	// over IMS or NAS, whichever way it was sent.
	var smsRouter *route.Router
	var imsLink *sms.IMSLink
	var smsfApp *smsf.SMSF
	if slices.Contains(names, "sms") || slices.Contains(names, "smsf") {
		imsLink = &sms.IMSLink{SCSCF: cfg.SMS.SCSCF, SelfIP: cfg.SMS.SelfIP, ViaPort: cfg.SMS.ViaPort, Domain: cfg.SMS.Domain,
			Answer: seconds(cfg.SMS.IMSAnswerSeconds)}
		if cfg.SMS.SCSCF != "" {
			_ = imsLink.Start() // a failed bind is logged; the link then sends blind, as setu always did
			closers = append(closers, imsLink.Close)
		}
		smsRouter = &route.Router{IMS: imsLink, Order: cfg.SMS.MTOrder, Deadline: seconds(cfg.SMS.DeadlineSeconds),
			SMSC: cfg.SMS.SMSCAddress}
		if slices.Contains(names, "smsf") {
			if err := cfg.CheckSMSFApp(); err != nil {
				log.Fatalf("setu: %v", err)
			}
			trusted, _ := cfg.SMSF.TrustedPrefixes() // CheckSMSFApp has parsed them
			smsfApp, err = smsf.New(smsf.Config{
				Listen: cfg.SMSF.Listen, APIRoot: cfg.SMSF.APIRoot, NFInstanceID: cfg.SMSF.NFInstanceID,
				NRF: cfg.SMSF.NRF, AMF: cfg.SMSF.AMF, Insecure: cfg.SMSF.Insecure, StateFile: cfg.SMSF.StateFile,
				ContextMaxAge: time.Duration(cfg.SMSF.ContextMaxAgeHours) * time.Hour,
				TC1:           seconds(cfg.SMSF.TC1Seconds), MaxRetrans: cfg.SMSF.MaxRetrans,
				PagingWait: seconds(cfg.SMSF.PagingWaitSeconds),
				PLMN:       []smsf.PLMN{{MCC: cfg.PLMN.MCC, MNC: cfg.PLMN.MNC}},
				Trusted:    trusted,
			}, core.Subscriber())
			if err != nil {
				log.Fatalf("setu: %v", err)
			}
			smsfApp.Router = smsRouter
			smsRouter.NAS, smsRouter.Dir = smsfApp, smsfApp
			closers = append(closers, smsfApp.Close) // NRF deregistration and the last state write
		}
	}

	for _, app := range names {
		switch app {
		case "rx":
			grants, restored, err := fabric.Open(cfg.Rx.WALPath)
			if err != nil {
				log.Fatalf("setu: rx WAL: %v", err)
			}
			if len(restored) > 0 {
				log.Printf("setu: rx restored %d live grant(s) from WAL", len(restored))
			}
			closers = append(closers, grants.Close)
			a := &rx.Adapter{
				Listen: cfg.Rx.Listen, OriginHost: cfg.Rx.OriginHost,
				OriginRealm: cfg.Rx.OriginRealm, HostIP: cfg.Rx.HostIP,
				Core: core, Grants: grants, Strict: cfg.Rx.Strict,
			}
			run("rx", a.Serve)
		case "cx":
			a := &cx.Adapter{
				Listen: cfg.Cx.Listen, OriginHost: cfg.Cx.OriginHost,
				OriginRealm: cfg.Cx.OriginRealm, HostIP: cfg.Cx.HostIP,
				SCSCFName:  cfg.Cx.SCSCF,
				PLMN:       ident.PLMN{MCC: cfg.PLMN.MCC, MNC: cfg.PLMN.MNC},
				Core:       core,
				AppServers: appServerTriggers(cfg.Cx.AppServers),
			}
			log.Printf("setu: cx profiles carry %d application-server rule(s)", len(a.AppServers))
			reg := a.InitMetrics()
			if cfg.Cx.Admin != "" {
				go func() {
					log.Printf("setu: cx admin (/metrics,/healthz) on %s", cfg.Cx.Admin)
					if err := http.ListenAndServe(cfg.Cx.Admin, reg.Mux("setu-cx", version)); err != nil {
						log.Printf("setu: cx admin: %v", err)
					}
				}()
			}
			run("cx", a.Serve)
		case "sms":
			a := &sms.Adapter{
				Listen: cfg.SMS.Listen, SCSCF: cfg.SMS.SCSCF,
				SelfIP: cfg.SMS.SelfIP, ViaPort: cfg.SMS.ViaPort, Domain: cfg.SMS.Domain,
				SMSC: cfg.SMS.SMSCAddress, Router: smsRouter, Link: imsLink,
			}
			run("sms", a.Serve)
		case "smsf":
			run("smsf", smsfApp.Serve)
		}
	}
	// Drain session state on SIGTERM so a restart does not resurrect grants.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	select {
	case err := <-errc:
		log.Fatalf("setu: app exited: %v", err)
	case sig := <-sigc:
		log.Printf("setu: %v — draining state and shutting down", sig)
		for _, c := range closers {
			if err := c(); err != nil {
				log.Printf("setu: close: %v", err)
			}
		}
		os.Exit(0)
	}
}

// validApps are the apps -apps can name.
var validApps = []string{"rx", "cx", "sms", "smsf"}

// parseApps reads the -apps list. An unknown name stops setu before any app starts.
func parseApps(list string) ([]string, error) {
	var out []string
	for _, a := range strings.Split(list, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !slices.Contains(validApps, a) {
			return nil, fmt.Errorf("unknown app %q (valid: %s)", a, strings.Join(validApps, ","))
		}
		out = append(out, a)
	}
	return out, nil
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

func buildConnector(cfg *config.Config) (api.CoreConnector, error) {
	switch cfg.Core {
	case "sdcore":
		dialect := rx2n5.DefaultConfig()
		if cfg.SDCore.DialectFile != "" {
			b, err := os.ReadFile(cfg.SDCore.DialectFile)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(b, &dialect); err != nil {
				return nil, err
			}
		}
		return sdcore.New(sdcore.Config{
			PCFBase: cfg.SDCore.PCF, UDRBase: cfg.SDCore.UDR,
			PLMNID: cfg.PLMN.MCC + cfg.PLMN.MNC, Insecure: cfg.SDCore.Insecure,
			NotifURI: cfg.SDCore.NotifURI, NotifListen: cfg.SDCore.NotifListen,
			Dialect: dialect,
		})
	default:
		return nil, &unknownCore{cfg.Core}
	}
}

type unknownCore struct{ name string }

func (e *unknownCore) Error() string {
	return "unknown core connector " + e.name + " (registered: sdcore)"
}

// appServerTriggers turns setu.json's cx.appServers (checked when the file was loaded) into the rules imssub.Build
// writes into every profile as iFC.
func appServerTriggers(list []config.AppServer) []imssub.AppServerTrigger {
	out := make([]imssub.AppServerTrigger, 0, len(list))
	for _, a := range list {
		out = append(out, imssub.AppServerTrigger{
			ServerName: a.ServerName, Method: a.Method, RequestURI: a.RequestURI, SessionCase: a.SessionCase,
			DefaultHandling: a.DefaultHandling, Priority: a.Priority,
		})
	}
	return out
}
