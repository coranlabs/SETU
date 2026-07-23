// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package udr

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// Well-known free5GC/OMEC test vector (public, not a secret): K and OPc used by
// the default SD-Core test subscriber.
const (
	wantK   = "8baf473f2f8fd09487cccbd7097c6862"
	wantOPc = "8e27b6af0e692e750f32667a3b14605d"
)

func TestParseAuthSubStandardShape(t *testing.T) {
	av, err := ParseAuthSubscription(read(t, "authsub_standard.json"))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(av.K) != wantK {
		t.Fatalf("K = %x, want %s", av.K, wantK)
	}
	if hex.EncodeToString(av.OPc) != wantOPc {
		t.Fatalf("OPc = %x, want %s", av.OPc, wantOPc)
	}
	if hex.EncodeToString(av.AMF) != "8000" {
		t.Fatalf("AMF = %x, want 8000", av.AMF)
	}
	if len(av.SQN) != 6 || hex.EncodeToString(av.SQN) != "16f3b3f70fc2" {
		t.Fatalf("SQN = %x, want 16f3b3f70fc2", av.SQN)
	}
}

// The IOS-MCN "enc" shape (encPermanentKey/encOpcKey + sequenceNumber object)
// must parse to the same material.
func TestParseAuthSubEncShape(t *testing.T) {
	av, err := ParseAuthSubscription(read(t, "authsub_enc.json"))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(av.K) != wantK || hex.EncodeToString(av.OPc) != wantOPc {
		t.Fatalf("enc shape K/OPc mismatch: K=%x OPc=%x", av.K, av.OPc)
	}
	if hex.EncodeToString(av.SQN) != "000000000021" {
		t.Fatalf("SQN = %x, want 000000000021", av.SQN)
	}
}

func TestParseAuthSubErrors(t *testing.T) {
	if _, err := ParseAuthSubscription([]byte(`{"opc":{"opcValue":"8e27b6af0e692e750f32667a3b14605d"}}`)); err == nil {
		t.Fatal("expected error when K missing")
	}
	if _, err := ParseAuthSubscription([]byte(`{"permanentKey":{"permanentKeyValue":"xyz"}}`)); err == nil {
		t.Fatal("expected error on non-hex K")
	}
	if _, err := ParseAuthSubscription([]byte(`{"permanentKey":{"permanentKeyValue":"deadbeef"},"opc":{"opcValue":"8e27b6af0e692e750f32667a3b14605d"}}`)); err == nil {
		t.Fatal("expected error on wrong-length K")
	}
}

func TestParseMSISDN(t *testing.T) {
	m, err := ParseMSISDN(read(t, "amdata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m != "9000000001" {
		t.Fatalf("MSISDN = %q, want 9000000001", m)
	}
	if _, err := ParseMSISDN([]byte(`{"gpsis":["extid-foo@bar"]}`)); err == nil {
		t.Fatal("expected error when no msisdn- GPSI present")
	}
}
