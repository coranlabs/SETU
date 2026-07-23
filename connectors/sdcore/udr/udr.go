// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package udr parses the SD-Core UDR (Nudr_DataRepository) responses the Cx bridge
// needs: the AuthenticationSubscription (K/OPc/AMF/SQN) and the am-data GPSIs (MSISDN).
//
// It accepts BOTH authentication-subscription shapes seen in the field (standard
// S-CSCF's fetchUdr): the standard permanentKey.permanentKeyValue / opc.opcValue form
// and the IOS-MCN encPermanentKey / encOpcKey form; sequenceNumber may be a scalar or
// an object with an sqn field.
package udr

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// AuthVector is the material MILENAGE needs, as raw bytes.
type AuthVector struct {
	K   []byte // 16 bytes
	OPc []byte // 16 bytes
	AMF []byte // 2 bytes
	SQN []byte // 6 bytes (48-bit)
}

// rawAuthSub captures the union of the shapes UDR returns.
type rawAuthSub struct {
	AuthenticationManagementField string `json:"authenticationManagementField"`

	// standard form
	PermanentKey *struct {
		PermanentKeyValue string `json:"permanentKeyValue"`
	} `json:"permanentKey"`
	Opc *struct {
		OpcValue string `json:"opcValue"`
	} `json:"opc"`

	// IOS-MCN "enc" form
	EncPermanentKey string `json:"encPermanentKey"`
	EncOpcKey       string `json:"encOpcKey"`

	// sequenceNumber: scalar string OR object {sqn}
	SequenceNumber json.RawMessage `json:"sequenceNumber"`
}

// ParseAuthSubscription extracts the auth vector material from a UDR
// authentication-subscription document, tolerating both shapes.
func ParseAuthSubscription(b []byte) (AuthVector, error) {
	var r rawAuthSub
	if err := json.Unmarshal(b, &r); err != nil {
		return AuthVector{}, fmt.Errorf("udr: bad authentication-subscription JSON: %w", err)
	}

	kHex := r.EncPermanentKey
	if kHex == "" && r.PermanentKey != nil {
		kHex = r.PermanentKey.PermanentKeyValue
	}
	opcHex := r.EncOpcKey
	if opcHex == "" && r.Opc != nil {
		opcHex = r.Opc.OpcValue
	}
	if kHex == "" {
		return AuthVector{}, fmt.Errorf("udr: no permanent key (K) in authentication-subscription")
	}
	if opcHex == "" {
		return AuthVector{}, fmt.Errorf("udr: no OPc in authentication-subscription")
	}

	k, err := decodeHexLen(kHex, 16, "K")
	if err != nil {
		return AuthVector{}, err
	}
	opc, err := decodeHexLen(opcHex, 16, "OPc")
	if err != nil {
		return AuthVector{}, err
	}
	amf, err := decodeHexLen(orDefault(r.AuthenticationManagementField, "8000"), 2, "AMF")
	if err != nil {
		return AuthVector{}, err
	}
	sqnHex, err := parseSQN(r.SequenceNumber)
	if err != nil {
		return AuthVector{}, err
	}
	sqn, err := decodeSQN(sqnHex)
	if err != nil {
		return AuthVector{}, err
	}
	return AuthVector{K: k, OPc: opc, AMF: amf, SQN: sqn}, nil
}

func parseSQN(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "000000000000", nil // default start-of-life SQN
	}
	// try scalar string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	// try object { "sqn": "..." }
	var o struct {
		Sqn string `json:"sqn"`
	}
	if err := json.Unmarshal(raw, &o); err == nil && o.Sqn != "" {
		return o.Sqn, nil
	}
	return "", fmt.Errorf("udr: unrecognised sequenceNumber form")
}

// decodeSQN normalises an SQN hex string to exactly 6 bytes (48-bit).
func decodeSQN(sqnHex string) ([]byte, error) {
	sqnHex = strings.TrimPrefix(strings.ToLower(sqnHex), "0x")
	if len(sqnHex) > 12 {
		return nil, fmt.Errorf("udr: SQN hex too long (%d)", len(sqnHex))
	}
	for len(sqnHex) < 12 {
		sqnHex = "0" + sqnHex
	}
	return decodeHexLen(sqnHex, 6, "SQN")
}

func decodeHexLen(s string, n int, name string) ([]byte, error) {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x")
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("udr: %s not hex: %w", name, err)
	}
	if len(b) != n {
		return nil, fmt.Errorf("udr: %s must be %d bytes, got %d", name, n, len(b))
	}
	return b, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---- am-data (MSISDN / GPSI) ----

type rawAmData struct {
	Gpsis []string `json:"gpsis"`
}

// ParseMSISDN extracts the first msisdn- GPSI from a UDR am-data document and
// returns the bare MSISDN digits.
func ParseMSISDN(b []byte) (string, error) {
	var r rawAmData
	if err := json.Unmarshal(b, &r); err != nil {
		return "", fmt.Errorf("udr: bad am-data JSON: %w", err)
	}
	for _, g := range r.Gpsis {
		if strings.HasPrefix(g, "msisdn-") {
			return strings.TrimPrefix(g, "msisdn-"), nil
		}
	}
	// The SD-Core webconsole writes gpsis as bare digits (no msisdn- prefix);
	// accept an all-digit GPSI as an MSISDN.
	for _, g := range r.Gpsis {
		if g != "" && strings.Trim(g, "0123456789") == "" {
			return g, nil
		}
	}
	return "", fmt.Errorf("udr: no msisdn- GPSI in am-data")
}
