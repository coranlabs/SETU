// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package diameter

import "testing"

// The decoder must never panic on hostile/malformed input (S1.1: any peer can
// connect, so the decoder is reachable by an attacker). Table of crafted-bad inputs.
func TestDecodeMalformedNeverPanics(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{1},
		make([]byte, 19), // shorter than header
		{1, 0, 0, 40, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},             // declared len 40 but buffer 16
		{2, 0, 0, 20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, // bad version
		// header ok (len 28) + an AVP claiming a huge length
		{1, 0, 0, 28, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 1, 0x40, 0xff, 0xff, 0xff},
		// header ok + AVP length < 8 (invalid)
		{1, 0, 0, 28, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 1, 0x40, 0, 0, 3},
		// vendor AVP flag set but length < 12
		{1, 0, 0, 28, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 1, 0x80, 0, 0, 8},
	}
	for i, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d panicked: %v", i, r)
				}
			}()
			_, _ = DecodeMessage(c) // must return an error, never panic
			_, _ = DecodeAVPs(c)
		}()
	}
}

// FuzzDecodeMessage runs the decoder over arbitrary bytes (its seed corpus runs in
// normal `go test`; `go test -fuzz` runs it continuously). It must never panic.
func FuzzDecodeMessage(f *testing.F) {
	f.Add([]byte{1, 0, 0, 20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add(Message{Flags: CmdRequest, Code: CmdAA, AppID: AppRx,
		AVPs: []AVP{Str(263, 0, true, "s"), U32(520, Vendor3GPP, true, 0)}}.Encode())
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("decoder panicked on %d bytes: %v", len(data), r)
			}
		}()
		if m, err := DecodeMessage(data); err == nil {
			_ = m.Encode() // re-encoding a successfully-decoded message must also be safe
		}
	})
}
