// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package qosconv is the CORRECTED bit-rate conversion for the SD-Core SMF, fixing
// finding B7 (docs/ios-mcn/99-findings-and-gaps.md): the shipped
// util.NormalizeBitRate hardcodes " Kbps" regardless of the real unit, so a GBR like
// "1.5 Mbps" is corrupted to "1 Kbps"; and BitRateTokbps used Atoi (can't parse a
// decimal). This package is the reference implementation + tests; the drop-in patch
// for the SMF is in deploy/sdcore-patches/smf-qos_convert.md.
package qosconv

import (
	"math"
	"strconv"
	"strings"
)

// BitRateTokbps parses a 3GPP bit-rate string ("<number> <unit>") to kbps. It
// tolerates a decimal value and preserves the unit's magnitude.
func BitRateTokbps(bitrate string) uint64 {
	fields := strings.Fields(bitrate)
	if len(fields) < 2 {
		return 0
	}
	val, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || val < 0 {
		return 0
	}
	var factor float64
	switch fields[1] {
	case "bps":
		factor = 1.0 / 1000
	case "Kbps":
		factor = 1
	case "Mbps":
		factor = 1000
	case "Gbps":
		factor = 1000 * 1000
	case "Tbps":
		factor = 1000 * 1000 * 1000
	default:
		return 0
	}
	return uint64(math.Round(val * factor))
}

// NormalizeBitRate trims a bit-rate string WITHOUT mangling the unit (the shipped
// version replaced the unit with "Kbps" whenever a decimal was present). The value
// and unit are preserved so BitRateTokbps computes the correct magnitude.
func NormalizeBitRate(br string) string {
	return strings.TrimSpace(br)
}
