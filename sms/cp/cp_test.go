// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package cp

import (
	"bytes"
	"testing"
)

// the three messages on the wire, as TS 24.011 8.1 lays them out
func TestEncodeGolden(t *testing.T) {
	rp := []byte{0x03, 0x07}
	for _, c := range []struct {
		name string
		got  []byte
		want []byte
	}{
		{"CP-DATA from the starter, TI 0", EncodeData(false, 0, rp), []byte{0x09, 0x01, 0x02, 0x03, 0x07}},
		{"CP-ACK answering, TI 0", EncodeAck(true, 0), []byte{0x89, 0x04}},
		{"CP-ERROR invalid TI, TI 0", EncodeError(false, 0, CauseInvalidTI), []byte{0x09, 0x10, 0x51}},
		{"CP-DATA answering, TI 3", EncodeData(true, 3, rp), []byte{0xb9, 0x01, 0x02, 0x03, 0x07}},
		{"CP-ACK from the starter, TI 6", EncodeAck(false, 6), []byte{0x69, 0x04}},
	} {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s: % x, want % x", c.name, c.got, c.want)
		}
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	rp := bytes.Repeat([]byte{0x5a}, MaxRPDU)
	for _, c := range []struct {
		name string
		in   []byte
		want Msg
	}{
		{"data", EncodeData(true, 5, []byte{0x00, 0x01, 0x02}), Msg{TIFlag: true, TI: 5, Type: TypeData, RPDU: []byte{0x00, 0x01, 0x02}}},
		{"data at the length limit", EncodeData(false, 2, rp), Msg{TI: 2, Type: TypeData, RPDU: rp}},
		{"ack", EncodeAck(false, 4), Msg{TI: 4, Type: TypeAck}},
		{"error", EncodeError(true, 6, CauseProtocolError), Msg{TIFlag: true, TI: 6, Type: TypeError, Cause: CauseProtocolError}},
	} {
		got, err := Decode(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.TIFlag != c.want.TIFlag || got.TI != c.want.TI || got.Type != c.want.Type || got.Cause != c.want.Cause ||
			!bytes.Equal(got.RPDU, c.want.RPDU) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestDecodeRefuses(t *testing.T) {
	long := append([]byte{0x09, 0x01, MaxRPDU + 1}, bytes.Repeat([]byte{0}, MaxRPDU+1)...)
	for _, c := range []struct {
		name string
		in   []byte
	}{
		{"TI 7 (extension)", []byte{0x79, 0x04}},
		{"TI 7 with flag", []byte{0xf9, 0x04}},
		{"wrong PD (5GMM)", []byte{0x7e, 0x04}},
		{"wrong PD (call control)", []byte{0x03, 0x01, 0x00}},
		{"LI longer than the rest", []byte{0x09, 0x01, 0x03, 0x00, 0x01}},
		{"LI shorter than the rest", []byte{0x09, 0x01, 0x01, 0x00, 0x01}},
		{"LI over 248", long},
		{"unknown type", []byte{0x09, 0x02}},
		{"empty", nil},
	} {
		if m, err := Decode(c.in); err == nil {
			t.Errorf("%s: accepted as %+v", c.name, m)
		}
	}
}

// a message cut anywhere is an error, never a panic
func TestDecodeTruncated(t *testing.T) {
	for _, full := range [][]byte{
		EncodeData(false, 1, []byte{0x00, 0x07, 0x00, 0x02, 0x91, 0xf7}),
		EncodeError(true, 1, CauseCongestion),
		EncodeAck(true, 1),
	} {
		for k := 0; k < len(full); k++ {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("Decode(% x) panicked: %v", full[:k], r)
					}
				}()
				if _, err := Decode(full[:k]); err == nil {
					t.Errorf("Decode(% x) accepted a cut message", full[:k])
				}
			}()
		}
	}
}
