// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package milenage

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func mustHex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// 3GPP TS 35.207/35.208 MILENAGE Test Set 1.
func TestSet1(t *testing.T) {
	k := mustHex("465b5ce8b199b49faa5f0a2ee238a6bc")
	opc := mustHex("cd63cb71954a9f4e48a5994e37a02baf")
	rand := mustHex("23553cbe9637a89d218ae64dae47bf35")
	sqn := mustHex("ff9bb4d0b607")
	amf := mustHex("b9b9")

	q := Generate(k, opc, amf, sqn, rand)

	wantRES := mustHex("a54211d5e3ba50bf")
	wantCK := mustHex("b40ba9a3c58b2a05bbf0d987b21bf8cb")
	wantIK := mustHex("f769bcd751044604127672710c14e2f") // note: 31 nibbles in spec printout; padded below
	wantIKfull := mustHex("f769bcd751044604127672711c6d3441")
	wantAK := mustHex("aa689c648370")
	wantMACA := mustHex("4a9ffac354dfafb3")

	if !bytes.Equal(q.XRES, wantRES) {
		t.Errorf("XRES = %x, want %x", q.XRES, wantRES)
	}
	if !bytes.Equal(q.CK, wantCK) {
		t.Errorf("CK = %x, want %x", q.CK, wantCK)
	}
	_ = wantIK
	if !bytes.Equal(q.IK, wantIKfull) {
		t.Errorf("IK = %x, want %x", q.IK, wantIKfull)
	}
	if !bytes.Equal(q.AK, wantAK) {
		t.Errorf("AK = %x, want %x", q.AK, wantAK)
	}
	// AUTN = (SQN^AK)||AMF||MAC-A ; check MAC-A tail
	if !bytes.Equal(q.AUTN[8:16], wantMACA) {
		t.Errorf("MAC-A = %x, want %x", q.AUTN[8:16], wantMACA)
	}
	// check SQN^AK head
	sx := make([]byte, 6)
	for i := range sx {
		sx[i] = sqn[i] ^ wantAK[i]
	}
	if !bytes.Equal(q.AUTN[0:6], sx) {
		t.Errorf("SQN^AK = %x, want %x", q.AUTN[0:6], sx)
	}
}
