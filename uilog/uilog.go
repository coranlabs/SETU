// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package uilog renders SETU's runtime events as clean, aligned, colored log
// lines for live demos and operations. One event == one line: a dim timestamp,
// a colored icon+tag column, then the human-readable detail. Noise (raw JSON
// bodies, per-request Diameter dumps) is deliberately kept out of this surface.
package uilog

import (
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	reset = "\033[0m"
	dim   = "\033[2m"
	bold  = "\033[1m"
)

const (
	Magenta = "\033[38;5;177m" // Cx registration / auth
	Green   = "\033[38;5;114m" // audio / bearer up
	Cyan    = "\033[38;5;80m"  // video
	Orange  = "\033[38;5;215m" // call teardown / warnings
	Yellow  = "\033[38;5;222m" // SMS
	Blue    = "\033[38;5;111m" // locate / generic media
	Red     = "\033[38;5;203m" // failures
)

func init() { log.SetFlags(0) } // we render our own timestamp

// Event prints one styled line:  HH:MM:SS  <icon> TAG      detail...
func Event(color, icon, tag, format string, args ...any) {
	ts := time.Now().Format("15:04:05")
	detail := fmt.Sprintf(format, args...)
	log.Printf("%s%s%s  %s %s%s%-8s%s %s", dim, ts, reset, icon, bold, color, tag, reset, detail)
}

// Short reduces a core session reference (often a full app-session URL) to its
// last path segment for compact display.
func Short[T ~string](s T) string {
	ref := string(s)
	if i := strings.LastIndexByte(ref, '/'); i >= 0 && i+1 < len(ref) {
		return ref[i+1:]
	}
	return ref
}
