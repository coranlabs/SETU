// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package milenage implements the 3GPP MILENAGE (TS 35.206) f1-f5 functions used to
// generate an IMS-AKA authentication quintet (RAND, AUTN, XRES, CK, IK) from a
// subscriber's K/OPc and the current SQN. It is the crypto behind the cx-hss-shim's
// Cx Multimedia-Auth-Answer, replacing Kamailio ims_auth's buggy av_mode=1 local path.
package milenage

import "crypto/aes"

// Quintet is one IMS-AKA authentication vector.
type Quintet struct {
	RAND []byte // 16
	AUTN []byte // 16 = (SQN^AK) || AMF || MAC-A
	XRES []byte // 8
	CK   []byte // 16
	IK   []byte // 16
	AK   []byte // 6  (exposed for SQN handling / resync)
}

func aesEnc(key, in []byte) []byte {
	c, _ := aes.NewCipher(key)
	out := make([]byte, 16)
	c.Encrypt(out, in)
	return out
}

func xor(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// rotBytes rotates a 16-byte block left by n bytes. All MILENAGE rotations r1..r4
// are multiples of 8 bits, so byte rotation is exact.
func rotBytes(in []byte, n int) []byte {
	out := make([]byte, 16)
	for i := 0; i < 16; i++ {
		out[i] = in[(i+n)%16]
	}
	return out
}

// Generate computes an AKA quintet. k/opc = 16 bytes, amf = 2, sqn = 6, rnd = 16.
func Generate(k, opc, amf, sqn, rnd []byte) Quintet {
	temp := aesEnc(k, xor(rnd, opc)) // TEMP = E_K(RAND ^ OPc)

	// f1: OUT1 = E_K( TEMP ^ rot(IN1 ^ OPc, 64b) ^ c1 ) ^ OPc ; c1 = 0
	in1 := make([]byte, 16)
	copy(in1[0:6], sqn)
	copy(in1[6:8], amf)
	copy(in1[8:14], sqn)
	copy(in1[14:16], amf)
	out1 := xor(aesEnc(k, xor(temp, rotBytes(xor(in1, opc), 8))), opc)
	macA := make([]byte, 8)
	copy(macA, out1[0:8])

	// f2/f5: OUT2 = E_K( rot(TEMP ^ OPc, 0) ^ c2 ) ^ OPc ; c2 = ...01
	t2 := rotBytes(xor(temp, opc), 0)
	t2[15] ^= 0x01
	out2 := xor(aesEnc(k, t2), opc)
	ak := make([]byte, 6)
	copy(ak, out2[0:6])
	xres := make([]byte, 8)
	copy(xres, out2[8:16])

	// f3: OUT3 = E_K( rot(TEMP ^ OPc, 32b) ^ c3 ) ^ OPc ; c3 = ...02
	t3 := rotBytes(xor(temp, opc), 4)
	t3[15] ^= 0x02
	ck := xor(aesEnc(k, t3), opc)

	// f4: OUT4 = E_K( rot(TEMP ^ OPc, 64b) ^ c4 ) ^ OPc ; c4 = ...04
	t4 := rotBytes(xor(temp, opc), 8)
	t4[15] ^= 0x04
	ik := xor(aesEnc(k, t4), opc)

	// AUTN = (SQN ^ AK) || AMF || MAC-A
	autn := make([]byte, 0, 16)
	autn = append(autn, xor(sqn, ak)...)
	autn = append(autn, amf...)
	autn = append(autn, macA...)

	return Quintet{RAND: rnd, AUTN: autn, XRES: xres, CK: ck, IK: ik, AK: ak}
}
