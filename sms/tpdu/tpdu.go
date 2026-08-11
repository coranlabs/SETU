// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package tpdu encodes and decodes short messages per 3GPP TS 23.040 (TPDU) and
// TS 24.011 (RP layer): SMS-SUBMIT in, SMS-DELIVER out, GSM 7-bit or UCS-2.
package tpdu

import (
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
	if udLen == 0 {
		return nil, fmt.Errorf("rp-ud empty")
	}
	if p+udLen > len(rp) {
		udLen = len(rp) - p
	}
	mo, err := decodeSubmit(rp[p : p+udLen])
	if mo != nil {
		mo.RPRef = rpRef
	}
	return mo, err
}

func decodeSubmit(t []byte) (*MO, error) {
	// Every read is bounds-checked: these bytes arrive from the network.
	at := func(i int) (byte, error) {
		if i < 0 || i >= len(t) {
			return 0, fmt.Errorf("tpdu truncated at octet %d of %d", i, len(t))
		}
		return t[i], nil
	}
	flags, err := at(0)
	if err != nil {
		return nil, err
	}
	if mti := flags & 0x03; mti != 0x01 {
		return nil, fmt.Errorf("tpdu is not an SMS-SUBMIT (MTI %d)", mti)
	}
	udhi := flags&0x40 != 0
	vpf := (flags >> 3) & 0x03

	i := 2 // skip TP-MR
	daDigits, err := at(i)
	if err != nil {
		return nil, err
	}
	i += 2 // address length + type-of-address
	daBytes := (int(daDigits) + 1) / 2
	if i+daBytes > len(t) {
		return nil, fmt.Errorf("tpdu truncated in TP-DA")
	}
	recipient := DecodeBCDAddr(t[i:i+daBytes], int(daDigits))
	i += daBytes

	i++ // TP-PID
	dcs, err := at(i)
	if err != nil {
		return nil, err
	}
	i++
	switch vpf {
	case 0x02:
		i++ // relative
	case 0x01, 0x03:
		i += 7 // enhanced or absolute
	}
	udlByte, err := at(i)
	if err != nil {
		return nil, err
	}
	udl := int(udlByte)
	i++
	ud := t[i:]

	udh := 0
	if udhi {
		if len(ud) == 0 {
			return nil, fmt.Errorf("tpdu declares a header but carries no user data")
		}
		udh = int(ud[0]) + 1
		if udh > len(ud) {
			return nil, fmt.Errorf("tpdu user-data header overruns the user data")
		}
	}

	var text string
	switch dcs & 0x0c {
	case 0x08:
		text = DecodeUCS2(ud[udh:])
	default:
		n := udl
		if udh > 0 {
			n = udl - (udh*8+6)/7
		}
		if n < 0 {
			n = 0
		}
		text = GSM7Unpack(ud[udh:], n)
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

// encodeSCTS renders TP-SCTS: 7 swapped-nibble BCD octets, timezone fixed at UTC.
func encodeSCTS(t time.Time) []byte {
	t = t.UTC() // the tz octet below is 0 (UTC); the fields must match it
	swap := func(v int) byte { return byte((v%10)<<4 | (v/10)%10) }
	return []byte{
		swap(t.Year() % 100), swap(int(t.Month())), swap(t.Day()),
		swap(t.Hour()), swap(t.Minute()), swap(t.Second()), 0x00,
	}
}

// EncodeDeliver builds an n->ms RP-DATA carrying an SMS-DELIVER, GSM 7-bit when
// the text allows it and UCS-2 otherwise.
func EncodeDeliver(sender, text string, scts time.Time) []byte {
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
	rp = append(rp, 0x01, 0x00)       // RP-DATA n->ms, ref 0
	rp = append(rp, 0x02, 0x91, 0xf7) // RP-OA: SMSC address
	rp = append(rp, 0x00)             // RP-DA: empty
	rp = append(rp, byte(len(t)))
	rp = append(rp, t...)
	return rp
}

// BuildRPAck builds an n->ms RP-ACK echoing the originating reference.
func BuildRPAck(ref byte) []byte { return []byte{0x03, ref} }
