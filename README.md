<div align="center">

# SETU

**Give any 5G core the services it doesn't have.**

*Release 1: IMS — voice, video and SMS on your 5G SA core, from day zero.*

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg)](go.mod)
[![Dependencies](https://img.shields.io/badge/dependencies-none-success.svg)](go.mod)
[![Release](https://img.shields.io/badge/release-1.0%20·%20IMS-green.svg)](#release-1--ims)
[![Status](https://img.shields.io/badge/status-verified%20in%20lab-yellow.svg)](#what-has-been-verified)

</div>

---

## In thirty seconds

Your 5G standalone core routes data beautifully. Then someone asks for a **phone call**.

Voice on 5G means VoNR, VoNR means IMS, and connecting a 5G core to an IMS means building a
translator between two protocol worlds that were never designed to meet — by hand, against one
vendor's quirks, every single time. For most open 5G cores there is no path to voice at all.

**SETU is that translator, built once and made reusable.** Put it between your core and an IMS,
point it at both, and you have voice, video and messaging — over the interfaces both sides already
speak, with your IMS untouched.

It is already proven: **SD-Core + Kamailio, VoNR, ViNR and SMS, end to end, on commercial
handsets over a real radio.**

*Setu* (सेतु) means *bridge*.

---

## Contents

- [The gap SETU fills](#the-gap-setu-fills)
- [What SETU is — and is not](#what-setu-is--and-is-not)
- [How it works](#how-it-works)
- [A call, end to end](#a-call-end-to-end)
- [Release 1 — IMS](#release-1--ims)
- [Quickstart](#quickstart)
- [Configuration](#configuration)
- [Deployment](#deployment)
- [Extending SETU](#extending-setu)
- [Roadmap](#roadmap)
- [FAQ](#faq)
- [Contributing](#contributing)
- [Acknowledgements](#acknowledgements)
- [License](#license)

---

## The gap SETU fills

A 5G SA core and an IMS speak different languages about the same things.

| | 5G core | IMS |
|---|---|---|
| **Transport** | HTTP/2, JSON | Diameter, SIP |
| **Policy / media** | `Npcf_PolicyAuthorization` (N5) | `Rx` (TS 29.214) |
| **Subscriber / auth** | `Nudr`, `Nausf` | `Cx` (TS 29.228/29.229) |
| **Messaging** | — | SMS over IP (TS 24.011/23.040) |

Nothing in either specification bridges them. So every deployment that wants voice builds its own
translator, and that translator goes the same way each time:

- the core's quirks get compiled into the translation logic, so **switching cores means a fork**
- session state lives in memory, so **a restart strands live sessions** on the core
- failures are swallowed, because the caller has no vocabulary for them, so **problems surface as
  mystery call failures**
- it works for exactly one deployment, and it is rewritten for the next

The integration is expensive to build, impossible to reuse, and brittle in the one place a network
cannot afford brittleness.

SETU exists so that this work is done once, in the open, and stays done.

## What SETU is — and is not

**SETU is** a signalling bridge that sits beside your core and an external system, speaks both
protocol worlds, and keeps the awkward parts — session state, failure semantics, per-vendor quirks —
in one place designed for them.

**SETU is not** an IMS, and not a 5G core. It does not replace Kamailio, and it does not replace your
PCF or UDR. It makes the two work together. Bring your own core, bring your own IMS.

Practically, that means:

- **no changes to your IMS** — SETU presents the Diameter interfaces an IMS already expects.
  Release 1 was proven against a stock Kamailio, configured only to point its `Rx` and `Cx` peers
  at SETU.
- **no HSS required** — subscriber data and authentication vectors come from the 5G core itself.
- **no proprietary interfaces** — SETU talks to the core over standard service-based APIs. What it
  asks of a core is exactly what 3GPP already specifies for IMS support (see below).

## How it works

SETU is built around a **narrow waist**. External systems attach through *adapters*; 5G cores attach
through *connectors*; a small shared model sits in the middle. Neither side ever sees the other's
dialect.

```
   ┌──────────────────────┐      ┌──────────────────────────┐      ┌──────────────────────┐
   │      5G SA CORE      │      │           SETU           │      │  EXTERNAL SYSTEMS    │
   │                      │      │                          │      │                      │
   │   PCF   UDR   UDM    │◄────►│  connectors ⇄ adapters   │◄────►│  IMS   ← Release 1   │
   │   AMF   SMF   UPF    │ SBI  │                          │ Rx   │  P/I/S-CSCF          │
   │                      │HTTP/2│      canonical model     │ Cx   │  VoNR · ViNR · SMS   │
   │  ▸ SD-Core  (today)  │      │      session fabric      │ SIP  │                      │
   │  ▸ free5GC, Open5GS, │      │      dialects            │      │  ▸ more systems and  │
   │    your core  →      │      │                          │      │    interfaces  →     │
   └──────────────────────┘      └──────────────────────────┘      └──────────────────────┘
        write a connector                                              write an adapter
```

Three ideas do the real work:

**Capability negotiation.** A connector *declares* what its core actually supports — in-place policy
update, reading sessions back, whether it issues authentication vectors itself. SETU adapts to the
answer instead of assuming, so a core with fewer features degrades cleanly rather than failing
strangely.

**Dialects.** Per-core quirks — bandwidth defaults, omitted fields, URL shapes — live in a JSON file
as *data*. Supporting a core's oddities never means forking a translator.

**A session fabric that survives.** Every authorized session is journaled to a write-ahead log, so
restarting SETU does not leave orphaned policy sessions accumulating inside your core.

### What your core needs to provide

SETU is a bridge, not a substitute for missing core functionality. Voice over 5G leans on parts of
the specifications that data-only deployments never exercise, and open cores implement them to
varying degrees. Your core needs:

| Capability | Specification | Why it matters |
|---|---|---|
| Media authorization over **N5** | TS 29.514 | SETU asks the PCF to authorize the call's media |
| **Dedicated QoS flows** from policy rules | TS 23.502, TS 24.501 | The PCF's decision must become a real GBR bearer via PDU session modification |
| **P-CSCF discovery** in the PDU session | TS 24.501 (PCO) | The handset has to learn where the IMS is before it can register |
| Subscriber data over **Nudr** | TS 29.505 | Authentication material and identities for `Cx` |
| **IMS voice indication** at registration | TS 24.501 | The handset only attempts VoNR when the network advertises support |

Gaps here show up as calls that set up in signalling but never get a bearer. When you meet one, the
fix belongs in the core — that is where the specification puts it — and it benefits every service on
that core, not just this bridge. SETU deliberately does not paper over such gaps: it would hide the
problem and lock you to a workaround.

## A call, end to end

What actually happens when a subscriber presses dial — SETU's part is the middle column:

```
 UE            Kamailio IMS           SETU                  5G Core
  │                  │                  │                      │
  │─ INVITE ────────►│                  │                      │
  │                  │─ Rx AAR ────────►│                      │
  │                  │                  │─ N5 app-session ────►│  PCF authorizes media
  │                  │                  │◄──────── 201 ────────│  PCF → SMF → dedicated
  │                  │◄──── 2001 ───────│                      │  5QI-1 GBR bearer to the UE
  │◄─ 183 / 200 OK ──│                  │                      │
  │◄══════════════ voice over the guaranteed bearer ═══════════│
  │                  │                  │                      │
  │─ BYE ───────────►│─ Rx STR ────────►│─ delete ────────────►│  bearer released
```

Registration follows the same shape: the S-CSCF asks over `Cx`, SETU fetches the subscriber's key
material from the core's `Nudr`, computes the IMS-AKA vector, and answers — so the handset is
challenged and authenticated with no HSS anywhere in the picture.

## Release 1 — IMS

The first release connects a 5G SA core to an IMS core.

| Adapter | Specification | What it gives you |
|---|---|---|
| **Rx** | TS 29.214 | Dedicated GBR bearers for media — 5QI-1 voice, 5QI-2 video |
| **Cx** | TS 29.228 / 29.229 | Registration and IMS-AKA authentication (UAR, LIR, SAR, MAR) from core subscriber data |
| **SMS over IP** | TS 24.011 / 23.040 | Mobile-originated and terminated messaging, GSM 7-bit and UCS-2 |

**Shipping connector:** OMEC / Aether **SD-Core**, running with full N5 policy authorization,
dedicated QoS flow establishment and P-CSCF discovery enabled.

### What has been verified

Tested end to end in a lab: commercial handsets → LiteON O-RU 5G SA radio → SD-Core → SETU →
Kamailio P/I/S-CSCF.

| | |
|---|---|
| ✅ **Registration** | Full IMS-AKA cycle — UAR → MAR → 401 challenge → SAR → 200, accepted by the handset |
| ✅ **VoNR** | Dedicated 5QI-1 GBR bearer established per call and released on hangup |
| ✅ **ViNR** | 5QI-2 video bearer alongside voice |
| ✅ **SMS** | Both directions, GSM 7-bit and UCS-2 (emoji), with submit reports |
| ✅ **Teardown** | Clean release, no policy-rule accumulation across repeated calls |
| ✅ **Restart safety** | Process restarted mid-session without stranding sessions on the core |

### What is lab-grade

Read this before putting it anywhere that matters:

- **single instance** — no clustering, no failover
- **TLS verification is disabled in the example configuration**, for self-signed lab certificates
- **`strict` mode is off by default** — Rx answers success even when the core rejects, matching the
  behaviour of the hand-built bridges SETU replaces. Switch it on for real TS 29.214 error codes.
- measured at roughly 75 media-authorization cycles per second on one node; not capacity-tested
  beyond that

### What is not built yet

- **A second core connector.** The pluggable design is real and contracted, but until a connector for
  another core exists, treat portability as an architectural claim rather than a demonstrated one.
- Core-initiated teardown (`RAR` / `ASR` toward the IMS) — notifications arrive but are not yet
  translated back.
- In-place session update: a re-negotiation creates a new session instead of modifying one.
- Inter-operator interworking (SEPP / N32).

## Quickstart

Go 1.26+ or Docker. No external dependencies — the tree builds against the standard library,
offline.

```bash
git clone https://github.com/coranlabs/SETU.git
cd setu
go test ./...
go build -o setu ./cmd/setu
```

Point it at your core and your IMS:

```bash
cp deploy/setu.example.json setu.json
# set: sdcore.pcf, sdcore.udr, and the host/S-CSCF addresses
./setu -config setu.json -apps rx,cx,sms
```

Then tell your IMS where SETU is — in Kamailio, that means aiming the `cdp` peers for `Rx` and `Cx`
at SETU's addresses. No other IMS change is required.

SETU refuses to start when core endpoints are missing. That is deliberate: a stale built-in address
fails silently and costs hours to find.

## Configuration

One JSON file:

```json
{
  "plmn": { "mcc": "001", "mnc": "01" },
  "core": "sdcore",
  "sdcore": {
    "pcf": "https://pcf.example.net:29507",
    "udr": "https://udr.example.net:29504",
    "notifURI": "http://setu.example.net:7777/notif",
    "notifListen": ":7777"
  },
  "rx":  { "listen": ":3868", "hostIP": "203.0.113.10", "walPath": "/var/lib/setu/rx-grants.wal" },
  "cx":  { "listen": ":3869", "hostIP": "203.0.113.10", "scscf": "sip:203.0.113.10:6060" },
  "sms": { "listen": "127.0.0.1:8090", "scscf": "203.0.113.10:6060", "selfIP": "203.0.113.10" }
}
```

| Key | Notes |
|---|---|
| `plmn` | The home domain is derived from it per TS 23.003 unless you set `domain` explicitly |
| `core` | Selects the connector |
| `walPath` | Enables the write-ahead log. Leave empty for memory-only; set it wherever a restart must not strand sessions |
| `rx.strict` | `false` (default) answers 2001 always; `true` returns real TS 29.214 error codes |
| `-apps` | Run all adapters in one process, or split them: `-apps rx`, `-apps cx,sms` |

Per-core behaviour lives in [`dialects/`](dialects/) — see [`dialects/sdcore.json`](dialects/sdcore.json).

## Deployment

**Docker**, host networking — the adapters bind well-known signalling ports and the SMS ingest must
be reachable on host loopback:

```bash
docker build -f deploy/Dockerfile.build -t setu:1.0 .
docker run -d --name setu --network host --restart unless-stopped --stop-timeout 15 \
  -v /etc/setu/setu.json:/etc/setu/setu.json:ro \
  -v /var/lib/setu:/var/lib/setu \
  setu:1.0
```

`docker stop` sends `SIGTERM`, which drains session state before exit. Keep the write-ahead log on a
host volume so it survives container replacement.

**systemd:** see [`deploy/setu.service`](deploy/setu.service).

**Observability:** Prometheus metrics and health on the admin listener (`cx.admin`, default `:9102`).

## Extending SETU

Two extension points, deliberately symmetrical.

### Add a 5G core — write a connector

```go
type CoreConnector interface {
    Name() string
    Capabilities() Caps          // what this core actually supports
    Policy() PolicyBackend       // authorize, update, revoke media
    Subscriber() SubscriberBackend
    Auth() AuthBackend           // authentication vectors
    Events() EventSource         // core-initiated notifications
}
```

Declare capabilities honestly and the platform adapts around them:

```go
func (c *Connector) Capabilities() api.Caps {
    return api.Caps{
        PolicyUpdate: false,            // no in-place update: fall back to delete + create
        AuthVector:   api.AuthCoreUEAU, // this core issues vectors itself
    }
}
```

Start from [`connectors/sdcore/`](connectors/sdcore/), keep core-specific constants in a dialect
file, and test against a fake core with `httptest` — see
[`connectors/sdcore/connector_test.go`](connectors/sdcore/connector_test.go).

### Add an external system — write an adapter

An adapter terminates whatever protocol that system speaks and expresses it in the shared model. It
needs no knowledge of any core, and every existing connector works with it on day one. See
[`adapters/rx/`](adapters/rx/) for the pattern.

## Roadmap

| Release | Focus |
|---|---|
| **1.0** | **IMS on SD-Core — Rx, Cx, SMS** *(this release)* |
| 1.1 | Core-initiated teardown (RAR/ASR), in-place session update, `strict` by default |
| 1.2 | Second core connector; connector conformance suite |
| 2.0 | Inter-operator interworking — SEPP / N32 |
| later | Further external interfaces, out-of-process connectors in any language |

The direction is constant: **one bridge, more things plugged into it.**

## FAQ

**Does SETU replace my IMS?**
No. It connects your 5G core to an IMS you already run. Kamailio is what Release 1 was proven
against; anything speaking standard Rx and Cx should work.

**Do I need an HSS?**
No. SETU answers Cx from the 5G core's own subscriber data and generates IMS-AKA vectors from it.

**Will it work with my 5G core?**
If your core exposes standard SBI interfaces, it needs a connector — typically a few hundred lines.
SD-Core ships today; free5GC and Open5GS are on the roadmap. If you write one, please contribute it.

**Do I have to modify my core or my IMS?**
Your IMS, no — Release 1 ran against a stock Kamailio with only peer configuration changed. Your
core, only if it does not yet implement the standard IMS-facing behaviour listed under
[what your core needs to provide](#what-your-core-needs-to-provide); voice exercises paths that
data-only deployments often leave incomplete.

**Is it production-ready?**
Not yet — see [what is lab-grade](#what-is-lab-grade). It is a working, verified foundation, and
we would rather say so plainly than have you discover the limits during an outage.

**Why Go, with no dependencies?**
A signalling element on the call path should be auditable and buildable offline, years from now,
without a supply chain behind it.

## Contributing

Contributions are welcome — **connectors for other 5G cores most of all**, since that is what turns
this from a working bridge into a portable one.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the workflow and sign-off requirement, and
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for community expectations. Report security issues
privately per [SECURITY.md](SECURITY.md).

## Acknowledgements

Built on the shoulders of the open telecom community — the **Kamailio** project, whose CSCFs are the
IMS side of Release 1; **OMEC / Aether SD-Core**, the first core SETU connects to; and **3GPP**,
whose specifications made an interoperable bridge possible at all.

## License

Copyright 2026 Coran Labs Private Limited.

Licensed under the Apache License, Version 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).

SETU interoperates with GPL-licensed software such as Kamailio over network protocols only. No
GPL-licensed code is included or linked.
