// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package cp is the SMS control-protocol layer of TS 24.011 (clause 7.2, coding in clause 8.1): the CP-DATA, CP-ACK
// and CP-ERROR messages that carry an RP message over NAS between the phone and the SMSF. Over IMS there is no CP
// layer (the SIP MESSAGE carries the RP message directly), so only the SMSF uses this package.
package cp

import (
	"errors"
	"fmt"
)

// PD is the protocol discriminator for SMS messages (TS 24.007 11.2.3.1.1), the low nibble of octet 1.
const PD = 0x09

// The three CP message types (TS 24.011 8.1.3).
const (
	TypeData  = 0x01
	TypeAck   = 0x04
	TypeError = 0x10
)

// MaxRPDU is the longest RP message a CP-DATA may carry (TS 24.011 8.1.4.1: CP-User data is at most 249 octets with
// its length octet).
const MaxRPDU = 248

// CP-Cause values (TS 24.011 8.1.4.2, Table 8.2). A receiver treats any other value as CauseProtocolError.
const (
	CauseNetworkFailure       = 17
	CauseCongestion           = 22
	CauseInvalidTI            = 81
	CauseSemanticallyWrong    = 95
	CauseInvalidMandatoryIE   = 96
	CauseUnknownMessageType   = 97
	CauseNotCompatibleInState = 98
	CauseUnknownIE            = 99
	CauseProtocolError        = 111
)

// Msg is one decoded CP message. TIFlag is false when the sender started the transaction and true when it answers
// the side that did (TS 24.007 11.2.3.1.3). RPDU is set for CP-DATA, Cause for CP-ERROR.
type Msg struct {
	TIFlag bool
	TI     uint8
	Type   uint8
	RPDU   []byte
	Cause  uint8
}

// ErrShort is returned when a message ends before a field it must carry.
var ErrShort = errors.New("cp: message too short")

// Decode parses one CP message. It refuses anything that is not a well-formed SMS CP message, so the caller can
// answer the phone with a CP-ERROR or a 400 instead of acting on a guess.
func Decode(b []byte) (Msg, error) {
	if len(b) < 2 {
		return Msg{}, ErrShort
	}
	if b[0]&0x0f != PD {
		return Msg{}, fmt.Errorf("cp: protocol discriminator %#x, want %#x (SMS)", b[0]&0x0f, PD)
	}
	m := Msg{TIFlag: b[0]&0x80 != 0, TI: (b[0] >> 4) & 0x07, Type: b[1]}
	// TI value 7 announces an extended TI octet (TS 24.007 11.2.3.1.3), which SMS does not use.
	if m.TI == 7 {
		return Msg{}, fmt.Errorf("cp: TI value 7 (extension) is not valid for SMS")
	}
	switch m.Type {
	case TypeData:
		if len(b) < 3 {
			return Msg{}, ErrShort
		}
		li := int(b[2])
		if li > MaxRPDU {
			return Msg{}, fmt.Errorf("cp: CP-User data length %d is over %d", li, MaxRPDU)
		}
		if li != len(b)-3 {
			return Msg{}, fmt.Errorf("cp: CP-User data length %d, but %d octets follow", li, len(b)-3)
		}
		m.RPDU = append([]byte(nil), b[3:]...)
	case TypeAck:
	case TypeError:
		if len(b) < 3 {
			return Msg{}, ErrShort
		}
		m.Cause = b[2]
	default:
		return Msg{}, fmt.Errorf("cp: unknown message type %#x", m.Type)
	}
	return m, nil
}

// octet1 builds the TI flag, TI value and PD octet.
func octet1(flag bool, ti uint8) byte {
	o := (ti&0x07)<<4 | PD
	if flag {
		o |= 0x80
	}
	return o
}

// EncodeData builds a CP-DATA carrying rpdu. The caller keeps rpdu within MaxRPDU.
func EncodeData(flag bool, ti uint8, rpdu []byte) []byte {
	out := make([]byte, 0, 3+len(rpdu))
	out = append(out, octet1(flag, ti), TypeData, byte(len(rpdu)))
	return append(out, rpdu...)
}

// EncodeAck builds a CP-ACK.
func EncodeAck(flag bool, ti uint8) []byte { return []byte{octet1(flag, ti), TypeAck} }

// EncodeError builds a CP-ERROR with the given cause.
func EncodeError(flag bool, ti uint8, cause uint8) []byte {
	return []byte{octet1(flag, ti), TypeError, cause}
}
