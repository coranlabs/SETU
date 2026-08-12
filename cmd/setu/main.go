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
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/coranlabs/SETU/adapters/cx"
	"github.com/coranlabs/SETU/adapters/rx"
	"github.com/coranlabs/SETU/adapters/sms"
	api "github.com/coranlabs/SETU/api/v1"
	"github.com/coranlabs/SETU/chassis/config"
	"github.com/coranlabs/SETU/connectors/sdcore"
	"github.com/coranlabs/SETU/connectors/sdcore/rx2n5"
	"github.com/coranlabs/SETU/fabric"
	"github.com/coranlabs/SETU/ims/ident"
)

const version = "0.1.0"

func main() {
	cfgPath := flag.String("config", "/etc/setu/setu.json", "path to setu.json")
	apps := flag.String("apps", "rx,cx,sms", "comma-separated apps to run: rx,cx,sms")
	flag.Parse()

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

	errc := make(chan error, 3)
	run := func(name string, f func() error) {
		go func() {
			log.Printf("setu: starting %s", name)
			errc <- f()
		}()
	}
	var closers []func() error

	for _, app := range strings.Split(*apps, ",") {
		switch strings.TrimSpace(app) {
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
				SCSCFName: cfg.Cx.SCSCF,
				PLMN:      ident.PLMN{MCC: cfg.PLMN.MCC, MNC: cfg.PLMN.MNC},
				Core:      core,
			}
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
			}
			run("sms", a.Serve)
		case "":
		default:
			log.Fatalf("setu: unknown app %q (valid: rx,cx,sms)", app)
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
