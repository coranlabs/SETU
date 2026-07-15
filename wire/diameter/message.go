// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package diameter

import (
	"encoding/binary"
	"fmt"
)

// Command flag bits (RFC 6733 §3).
const (
	CmdRequest    = 0x80 // 'R'
	CmdProxiable  = 0x40 // 'P'
	CmdError      = 0x20 // 'E'
	CmdRetransmit = 0x10 // 'T'
)

// Diameter application-ids and command codes used by the IMS interfaces.
const (
	AppCommon  uint32 = 0        // Diameter base (CER/DWR/DPR)
	AppCx      uint32 = 16777216 // 3GPP Cx/Dx
	AppRx      uint32 = 16777236 // 3GPP Rx
	Vendor3GPP uint32 = 10415

	CmdCE  uint32 = 257 // Capabilities-Exchange
	CmdDW  uint32 = 280 // Device-Watchdog
	CmdDP  uint32 = 282 // Disconnect-Peer
	CmdAA  uint32 = 265 // Rx AA-Request/Answer
	CmdRA  uint32 = 258 // Re-Auth
	CmdAS  uint32 = 274 // Abort-Session
	CmdST  uint32 = 275 // Session-Termination
	CmdUAR uint32 = 300 // Cx User-Authorization
	CmdSAR uint32 = 301 // Cx Server-Assignment
	CmdLIR uint32 = 302 // Cx Location-Info
	CmdMAR uint32 = 303 // Cx Multimedia-Auth
	CmdRTR uint32 = 304 // Cx Registration-Termination
	CmdPPR uint32 = 305 // Cx Push-Profile
)

// Message is a Diameter message (RFC 6733 §3).
type Message struct {
	Flags    byte
	Code     uint32
	AppID    uint32
	HopByHop uint32
	EndToEnd uint32
	AVPs     []AVP
}

// IsRequest reports whether the R bit is set.
func (m Message) IsRequest() bool { return m.Flags&CmdRequest != 0 }

// Encode serialises the message per RFC 6733 §3. The 20-byte header carries
// version=1, a 24-bit total length, command flags/code, and the ids.
func (m Message) Encode() []byte {
	var body []byte
	for _, a := range m.AVPs {
		body = append(body, a.Encode()...)
	}
	total := 20 + len(body)
	out := make([]byte, 20)
	out[0] = 1 // version
	out[1] = byte(total >> 16)
	out[2] = byte(total >> 8)
	out[3] = byte(total)
	out[4] = m.Flags
	out[5] = byte(m.Code >> 16)
	out[6] = byte(m.Code >> 8)
	out[7] = byte(m.Code)
	binary.BigEndian.PutUint32(out[8:12], m.AppID)
	binary.BigEndian.PutUint32(out[12:16], m.HopByHop)
	binary.BigEndian.PutUint32(out[16:20], m.EndToEnd)
	return append(out, body...)
}

// DecodeMessage parses a complete Diameter message from b.
func DecodeMessage(b []byte) (Message, error) {
	if len(b) < 20 {
		return Message{}, fmt.Errorf("diameter: message shorter than header (%d)", len(b))
	}
	if b[0] != 1 {
		return Message{}, fmt.Errorf("diameter: unsupported version %d", b[0])
	}
	total := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if total != len(b) {
		return Message{}, fmt.Errorf("diameter: declared length %d != buffer %d", total, len(b))
	}
	m := Message{
		Flags:    b[4],
		Code:     uint32(b[5])<<16 | uint32(b[6])<<8 | uint32(b[7]),
		AppID:    binary.BigEndian.Uint32(b[8:12]),
		HopByHop: binary.BigEndian.Uint32(b[12:16]),
		EndToEnd: binary.BigEndian.Uint32(b[16:20]),
	}
	avps, err := DecodeAVPs(b[20:])
	if err != nil {
		return Message{}, err
	}
	m.AVPs = avps
	return m, nil
}

// Answer builds a skeleton answer for this request: clears R, copies the
// application-id and the hop-by-hop/end-to-end ids (RFC 6733 §6.2).
func (m Message) Answer() Message {
	return Message{
		Flags:    m.Flags &^ CmdRequest,
		Code:     m.Code,
		AppID:    m.AppID,
		HopByHop: m.HopByHop,
		EndToEnd: m.EndToEnd,
	}
}
