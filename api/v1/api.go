// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package api defines the core-agnostic types exchanged between protocol adapters
// and core connectors. Adapters translate SIP/Diameter into these types;
// connectors translate them onto a particular 5G core's API.
//
// MediaGrant still carries the raw AAR in Raw so the sdcore connector can reuse
// the existing Rx->N5 translator. New connectors should use the typed fields.
package api

import (
	"context"
	"errors"

	"github.com/coranlabs/SETU/wire/diameter"
)

// Adapters map these onto their protocol's answer codes.
var (
	ErrUnavailable = errors.New("core unavailable")         // transport or timeout
	ErrRejected    = errors.New("core rejected")            // core understood and refused
	ErrNoSession   = errors.New("no core session for UE")   // no PDU session for the UE key
	ErrUnsupported = errors.New("unsupported by connector") // caller should fall back
	ErrNotFound    = errors.New("core session not found")
)

// ---- identity ----

type PLMNID struct{ MCC, MNC string }

// UEKey identifies the UE session to the core. IPv4 is the only field every core
// can bind on; terminating AARs often carry nothing else.
type UEKey struct {
	IPv4 string
	IPv6 string
	SUPI string // optional
	GPSI string // optional ("msisdn-<n>")
	DNN  string
	PLMN PLMNID
}

// ---- media authorization (the Rx AAR, distilled) ----

type ServiceClass int

const (
	ClassSignalling ServiceClass = iota
	ClassVoice
	ClassVideo
	ClassEmergency
)

// MediaGrant is one authorized media session for one UE.
type MediaGrant struct {
	ID      string // adapter-scoped id; the Rx Session-Id for Diameter
	UE      UEKey
	Service ServiceClass
	Raw     diameter.Message
}

// CoreRef references the core-side session. Its meaning is connector-specific;
// on SD-Core it is the N5 app-session Location.
type CoreRef string

// ---- auth (the Cx MAR, distilled) ----

type AuthChallenge struct {
	IMSI  string
	Count int
}

// AuthVector is one IMS-AKA quintet.
type AuthVector struct {
	RAND, AUTN, XRES, CK, IK []byte
}

// ---- SMS ----

type ShortMessage struct {
	From, To string // MSISDN digits
	TPDU     []byte // original RP-DATA preserved
	Text     string
	Ref      uint8 // RP-Message-Reference (echoed in the RP-ACK)
}

// ---- events (core -> bridge; phase 3 maps these to Rx RAR/ASR) ----

type EventKind string

const (
	EvNotifyRaw            EventKind = "notify-raw"
	EvTerminationRequested EventKind = "termination-requested"
	EvResourcesReleased    EventKind = "resources-released"
)

type SessionEvent struct {
	Grant  string
	Kind   EventKind
	Detail string
}

type AuthMode int

const (
	AuthLocalMilenage AuthMode = iota // bridge computes vectors from K/OPc and advances SQN
	AuthCoreUEAU                      // vectors delegated to the core's Nausf/Nudm
)

// Caps lets the fabric adapt to what a core actually implements.
type Caps struct {
	PolicyUpdate  bool // supports in-place update of a session
	PolicyRead    bool // supports reading a session back
	Notifications bool
	AuthVector    AuthMode
	SQNStep       int64 // SQN increment, AuthLocalMilenage only
}

// ---- connector contract ----

type PolicyBackend interface {
	Create(ctx context.Context, g *MediaGrant) (CoreRef, error)
	Update(ctx context.Context, ref CoreRef, g *MediaGrant) error // ErrUnsupported if the core has no update
	Delete(ctx context.Context, ref CoreRef) error                // idempotent
}

type SubscriberBackend interface {
	Exists(ctx context.Context, imsi string) (bool, error)
	MSISDN(ctx context.Context, imsi string) (string, error)
}

type AuthBackend interface {
	Vectors(ctx context.Context, ch AuthChallenge) ([]AuthVector, error)
}

type EventSource interface {
	Events() <-chan SessionEvent
}

// CoreConnector groups everything a single 5G core implementation provides.
// There is no SMS backend yet: MT delivery goes out over SIP from the sms adapter.
type CoreConnector interface {
	Name() string
	Capabilities() Caps
	Policy() PolicyBackend
	Subscriber() SubscriberBackend
	Auth() AuthBackend
	Events() EventSource
}
