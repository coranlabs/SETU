// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package tpdu is the 3GPP TS 23.040/24.011 short-message codec, re-homed verbatim
// from the e2e-proven sms-router (SMS "Hello" + RP-ACK proven live 2026-08-13).
// Two deliberate changes from the original:
//   - TP-SCTS is computed from an injected clock instead of the hardcoded
//     2025-08-06 bytes (52 80 60 00 00 00 00). Freeze the clock to that instant to
//     reproduce the historical bytes exactly (see tests).
//   - everything the adapter needs is exported.
package tpdu

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

var gsm7 = []rune("@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞ\x1bÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà")

func gsm7Index(r rune) (int, bool) {
	for i, c := range gsm7 {
		if c == r {
			return i, true
		}
	}
	return 0, false
}

// GSM7Unpack expands packed septets into text (udl = septet count).
func GSM7Unpack(data []byte, udl int) string {
	var out []rune
	bitpos := 0
	for i := 0; i < udl; i++ {
		bytePos := bitpos / 8
		shift := bitpos % 8
		if bytePos >= len(data) {
			break
		}
		val := int(data[bytePos]) >> shift
		if shift > 1 && bytePos+1 < len(data) {
			val |= int(data[bytePos+1]) << (8 - shift)
		}
		val &= 0x7f
		if val < len(gsm7) {
			out = append(out, gsm7[val])
		}
		bitpos += 7
	}
	return string(out)
}

// GSM7Pack packs text into septets, returning the bytes and the septet count.
func GSM7Pack(s string) ([]byte, int) {
	var septets []int
	for _, r := range s {
		if idx, ok := gsm7Index(r); ok {
			septets = append(septets, idx)
		} else {
			septets = append(septets, 0x3f) // '?'
		}
	}
	n := len(septets)
	buf := make([]byte, (n*7+7)/8+1)
	bitpos := 0
	for _, sv := range septets {
		bytePos := bitpos / 8
		shift := bitpos % 8
		buf[bytePos] |= byte((sv << shift) & 0xff)
		if shift > 1 {
			buf[bytePos+1] |= byte(sv >> (8 - shift))
		}
		bitpos += 7
	}
	return buf[:(n*7+7)/8], n
}

// DecodeBCDAddr decodes swapped-nibble BCD digits.
func DecodeBCDAddr(b []byte, numDigits int) string {
	var sb strings.Builder
	for i := 0; i < len(b) && sb.Len() < numDigits; i++ {
		lo := b[i] & 0x0f
		hi := b[i] >> 4
		if lo <= 9 {
			sb.WriteByte('0' + lo)
		}
		if sb.Len() < numDigits && hi <= 9 {
			sb.WriteByte('0' + hi)
		}
	}
	return sb.String()
}

// EncodeBCDAddr encodes digits as swapped-nibble BCD (odd length padded 0xF).
func EncodeBCDAddr(digits string) []byte {
	d := []byte(digits)
	out := make([]byte, (len(d)+1)/2)
	for i := 0; i < len(d); i++ {
		nib := d[i] - '0'
		if i%2 == 0 {
			out[i/2] = nib
		} else {
			out[i/2] |= nib << 4
		}
	}
	if len(d)%2 == 1 {
		out[len(out)-1] |= 0xf0
	}
	return out
}

// RP message types (TS 24.011 8.2.2): the MTI in bits 3-1 of the first RP octet. ms->n and n->ms differ, so an RP-ACK
// from the phone (2) and one to the phone (3) are told apart by the number alone.
const (
	RPDataMS  = 0 // RP-DATA, phone to network
	RPDataN   = 1 // RP-DATA, network to phone
	RPAckMS   = 2
	RPAckN    = 3
	RPErrorMS = 4
	RPErrorN  = 5
	RPSMMA    = 6 // the phone has memory for messages again
)

// RP-Cause values setu sends (TS 24.011 8.2.5.4, Table 8.4).
const (
	RPCauseRejected          = 21 // short message transfer rejected: the recipient refused it
	RPCauseDestOutOfOrder    = 27 // destination out of order: no domain reached the recipient
	RPCauseNetworkOutOfOrder = 38 // network out of order: the core failed, or the sender has no number
	RPCauseTemporaryFailure  = 41 // temporary failure: the delivery deadline ran out
	RPCauseSemanticallyWrong = 95
	RPCauseInvalidMandatory  = 96 // invalid mandatory information
	RPCauseUnknownType       = 97 // message type non-existent or not implemented
)

// ErrNotRPData is DecodeMO's answer for an RP message that is not a phone-to-network RP-DATA.
var ErrNotRPData = errors.New("tpdu: not an ms->n RP-DATA")

// RPType returns the RP message type (one of the RP* constants) of an RP message.
func RPType(rp []byte) (byte, error) {
	if len(rp) == 0 {
		return 0, fmt.Errorf("tpdu: empty RP message")
	}
	return rp[0] & 0x07, nil
}

// MO is a decoded mobile-originated short message.
type MO struct {
	Recipient string
	DCS       byte
	Text      string
	RPRef     byte // RP-Message-Reference from the MO RP-DATA (echoed in the RP-ACK)
}

// DecodeMO parses an ms->n RP-DATA carrying an SMS-SUBMIT.
func DecodeMO(rp []byte) (*MO, error) {
	if len(rp) < 3 {
		return nil, fmt.Errorf("rp too short")
	}
	if rp[0]&0x07 != RPDataMS {
		return nil, ErrNotRPData
	}
	rpRef := rp[1]
	p := 2
	if p >= len(rp) {
		return nil, fmt.Errorf("rp-oa")
	}
	p += 1 + int(rp[p])
	if p >= len(rp) {
		return nil, fmt.Errorf("rp-da")
	}
	p += 1 + int(rp[p])
	if p >= len(rp) {
		return nil, fmt.Errorf("rp-ud")
	}
	udLen := int(rp[p])
	p++
	if p+udLen > len(rp) {
		udLen = len(rp) - p
	}
	mo, err := decodeSubmit(rp[p : p+udLen])
	if mo != nil {
		mo.RPRef = rpRef
	}
	return mo, err
}

// decodeSubmit parses an SMS-SUBMIT (TS 23.040 9.2.2.2). Every field is checked against what is left, so a cut or
// lying message is an error, not a panic.
func decodeSubmit(t []byte) (*MO, error) {
	if len(t) < 3 {
		return nil, fmt.Errorf("tpdu short")
	}
	short := func(field string) error { return fmt.Errorf("tpdu: cut in %s", field) }
	i := 0
	flags := t[i]
	i++
	udhi := flags&0x40 != 0
	vpf := (flags >> 3) & 0x03
	i++ // TP-MR
	daDigits := int(t[i])
	i += 2 // len + TOA
	daBytes := (daDigits + 1) / 2
	if i+daBytes > len(t) {
		return nil, short("TP-DA")
	}
	recipient := DecodeBCDAddr(t[i:i+daBytes], daDigits)
	i += daBytes
	i++ // TP-PID
	if i >= len(t) {
		return nil, short("TP-PID/TP-DCS")
	}
	dcs := t[i]
	i++
	switch vpf {
	case 0x02:
		i++
	case 0x01, 0x03:
		i += 7
	}
	if i >= len(t) {
		return nil, short("TP-VP/TP-UDL")
	}
	udl := int(t[i])
	i++
	ud := t[i:]
	// UCS-2 and 8-bit data count octets, GSM 7-bit counts septets (TS 23.038 4)
	need := (udl*7 + 7) / 8
	if dcs&0x0c == 0x08 || dcs&0x0c == 0x04 {
		need = udl
	}
	if len(ud) < need {
		return nil, short("TP-UD")
	}
	ud = ud[:need]
	udh := 0
	if udhi && len(ud) > 0 {
		udh = int(ud[0]) + 1
		if udh > len(ud) {
			return nil, short("TP-UDH")
		}
	}
	var text string
	switch dcs & 0x0c {
	case 0x08:
		text = DecodeUCS2(ud[udh:])
	default:
		// GSM 7-bit: UDL counts septets from the start of the user data, the header and its fill bits included, and
		// the text starts on the first septet boundary after the header (TS 23.040 9.2.3.24). Reading it from the
		// octet after the header ignores the fill bits and garbles every character of a concatenated SMS.
		all := []rune(GSM7Unpack(ud, udl)) // one rune per septet
		skip := (udh*8 + 6) / 7
		if skip > len(all) {
			return nil, short("TP-UDH")
		}
		text = string(all[skip:])
	}
	return &MO{Recipient: recipient, DCS: dcs, Text: text}, nil
}

// DecodeUCS2 decodes UTF-16BE user data.
func DecodeUCS2(b []byte) string {
	var u16 []uint16
	for i := 0; i+1 < len(b); i += 2 {
		u16 = append(u16, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return string(utf16.Decode(u16))
}

// IsGSM7 reports whether every rune fits the GSM 7-bit default alphabet.
func IsGSM7(s string) bool {
	for _, r := range s {
		if _, ok := gsm7Index(r); !ok {
			return false
		}
	}
	return true
}

// UCS2Encode encodes text as UTF-16BE (emoji-safe).
func UCS2Encode(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(u16)*2)
	for _, u := range u16 {
		out = append(out, byte(u>>8), byte(u))
	}
	return out
}

// encodeSCTS renders TP-SCTS as 7 swapped-nibble BCD octets (yy MM dd hh mm ss tz).
// The timezone octet is 0x00, matching the proven deployment.
func encodeSCTS(t time.Time) []byte {
	t = t.UTC() // the tz octet below is 0 (UTC); the fields must match it
	swap := func(v int) byte { return byte((v%10)<<4 | (v/10)%10) }
	return []byte{
		swap(t.Year() % 100), swap(int(t.Month())), swap(t.Day()),
		swap(t.Hour()), swap(t.Minute()), swap(t.Second()), 0x00,
	}
}

// EncodeDeliver builds an n->ms RP-DATA carrying an SMS-DELIVER from sender to the
// UE, GSM-7 when possible, UCS-2 otherwise. scts stamps TP-SCTS; use a frozen clock
// in tests to reproduce historical bytes. It is EncodeDeliverWith with the proven
// system's SMSC "7" and RP-MR 0.
func EncodeDeliver(sender, text string, scts time.Time) []byte {
	return EncodeDeliverWith(sender, text, scts, "7", 0)
}

// EncodeDeliverWith is EncodeDeliver with the SMSC address carried in RP-OA and the RP-Message-Reference set by the
// caller, so a delivery report can be matched to the message it answers.
func EncodeDeliverWith(sender, text string, scts time.Time, smsc string, ref byte) []byte {
	var t []byte
	t = append(t, 0x04) // SMS-DELIVER, no more messages
	t = append(t, byte(len(sender)), 0x91)
	t = append(t, EncodeBCDAddr(sender)...)
	t = append(t, 0x00) // TP-PID
	if IsGSM7(text) {
		packed, septets := GSM7Pack(text)
		t = append(t, 0x00) // TP-DCS = GSM 7-bit
		t = append(t, encodeSCTS(scts)...)
		t = append(t, byte(septets))
		t = append(t, packed...)
	} else {
		ucs := UCS2Encode(text)
		t = append(t, 0x08) // TP-DCS = UCS2
		t = append(t, encodeSCTS(scts)...)
		t = append(t, byte(len(ucs)))
		t = append(t, ucs...)
	}
	var rp []byte
	rp = append(rp, RPDataN, ref) // RP-DATA n->ms
	// RP-OA: the SMSC, international (TS 24.011 8.2.5.1); "7" gives 02 91 f7, the proven system's bytes
	if sc := EncodeBCDAddr(strings.TrimPrefix(smsc, "+")); len(sc) > 0 {
		rp = append(rp, byte(1+len(sc)), 0x91)
		rp = append(rp, sc...)
	} else {
		rp = append(rp, 0x00)
	}
	rp = append(rp, 0x00) // RP-DA: empty
	rp = append(rp, byte(len(t)))
	rp = append(rp, t...)
	return rp
}

// BuildRPAck builds the n->ms RP-ACK (submit report) echoing the MO reference.
func BuildRPAck(ref byte) []byte { return []byte{0x03, ref} }

// BuildRPError builds the n->ms RP-ERROR echoing the MO reference, with a one-octet RP-Cause and no diagnostic.
func BuildRPError(ref, cause byte) []byte { return []byte{RPErrorN, ref, 0x01, cause & 0x7f} }

// DecodeRPReport reads the phone's answer to an MT RP-DATA: an ms->n RP-ACK (ok, with or without the optional
// RP-User data) or an ms->n RP-ERROR (not ok, with its RP-Cause).
func DecodeRPReport(rp []byte) (ok bool, ref, cause byte, err error) {
	if len(rp) < 2 {
		return false, 0, 0, fmt.Errorf("tpdu: RP report too short")
	}
	switch rp[0] & 0x07 {
	case RPAckMS:
		// the optional RP-User data (IEI 0x41) only carries a DELIVER-REPORT; the ACK itself is the answer
		return true, rp[1], 0, nil
	case RPErrorMS:
		if len(rp) < 4 || rp[2] < 1 || int(rp[2]) > len(rp)-3 {
			return false, rp[1], 0, fmt.Errorf("tpdu: RP-ERROR without a whole RP-Cause")
		}
		return false, rp[1], rp[3] & 0x7f, nil
	default:
		return false, rp[1], 0, fmt.Errorf("tpdu: RP type %d is not an ms->n RP-ACK or RP-ERROR", rp[0]&0x07)
	}
}
