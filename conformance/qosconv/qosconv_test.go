// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package qosconv

import "testing"

func TestBitRateTokbps(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"128 Kbps", 128},
		{"128.000000 Kbps", 128}, // free5GC form with trailing zeros
		{"1.5 Mbps", 1500},       // the B7 bug case: shipped code produced 1
		{"2 Mbps", 2000},
		{"1 Gbps", 1_000_000},
		{"1500000 bps", 1500},
		{"0 Kbps", 0},
		{"garbage", 0},
		{"5 Xbps", 0}, // unknown unit
	}
	for _, c := range cases {
		if got := BitRateTokbps(c.in); got != c.want {
			t.Errorf("BitRateTokbps(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// The end-to-end call the SMF makes — BitRateTokbps(NormalizeBitRate(x)) — must be
// correct for a non-Kbps decimal rate (the exact path that was corrupted).
func TestNormalizeThenConvertFixesB7(t *testing.T) {
	// shipped behaviour: NormalizeBitRate("1.5 Mbps") -> "1 Kbps" -> 1  (WRONG)
	// fixed behaviour:   NormalizeBitRate("1.5 Mbps") -> "1.5 Mbps" -> 1500
	if got := BitRateTokbps(NormalizeBitRate("1.5 Mbps")); got != 1500 {
		t.Fatalf("BitRateTokbps(NormalizeBitRate(\"1.5 Mbps\")) = %d, want 1500 (B7 not fixed)", got)
	}
	if got := BitRateTokbps(NormalizeBitRate("128.000000 Kbps")); got != 128 {
		t.Fatalf("128.000000 Kbps -> %d, want 128", got)
	}
	if got := NormalizeBitRate("2 Mbps"); got != "2 Mbps" {
		t.Fatalf("NormalizeBitRate mangled the unit: %q", got)
	}
}
