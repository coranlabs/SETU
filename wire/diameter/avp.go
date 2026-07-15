// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package diameter implements the parts of the Diameter base protocol (RFC 6733)
// needed by the SD-Core bridges: AVP and message encode/decode, including grouped
// AVPs, vendor-specific AVPs, flags and 32-bit boundary padding.
//
// It is dependency-free (standard library only) so it builds and tests offline.
package diameter

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// AVP flag bits (RFC 6733 §4.1).
const (
	FlagVendor    = 0x80 // 'V' — Vendor-Specific
	FlagMandatory = 0x40 // 'M' — Mandatory
	FlagProtected = 0x20 // 'P'
)

// AVP is a single Diameter Attribute-Value Pair. For grouped AVPs, Group holds
// the children and Data is ignored on encode.
type AVP struct {
	Code     uint32
	Flags    byte
	VendorID uint32
	Data     []byte
	Group    []AVP
	Grouped  bool
}

func pad4(n int) int {
	if r := n % 4; r != 0 {
		return 4 - r
	}
	return 0
}

// Encode serialises the AVP (with padding) per RFC 6733 §4.1.
func (a AVP) Encode() []byte {
	flags := a.Flags
	if a.VendorID != 0 {
		flags |= FlagVendor
	}
	var data []byte
	if a.Grouped {
		for _, c := range a.Group {
			data = append(data, c.Encode()...)
		}
	} else {
		data = a.Data
	}
	hdr := 8
	if flags&FlagVendor != 0 {
		hdr = 12
	}
	length := hdr + len(data)

	out := make([]byte, hdr)
	binary.BigEndian.PutUint32(out[0:4], a.Code)
	out[4] = flags
	// 24-bit length
	out[5] = byte(length >> 16)
	out[6] = byte(length >> 8)
	out[7] = byte(length)
	if flags&FlagVendor != 0 {
		binary.BigEndian.PutUint32(out[8:12], a.VendorID)
	}
	out = append(out, data...)
	out = append(out, make([]byte, pad4(length))...)
	return out
}

// DecodeAVPs parses a sequence of AVPs from b. Padding is consumed but the
// declared (unpadded) length governs each AVP's data slice.
func DecodeAVPs(b []byte) ([]AVP, error) {
	var avps []AVP
	off := 0
	for off < len(b) {
		if off+8 > len(b) {
			return nil, fmt.Errorf("diameter: truncated AVP header at %d", off)
		}
		code := binary.BigEndian.Uint32(b[off : off+4])
		flags := b[off+4]
		length := int(b[off+5])<<16 | int(b[off+6])<<8 | int(b[off+7])
		if length < 8 || off+length > len(b) {
			return nil, fmt.Errorf("diameter: bad AVP length %d at %d", length, off)
		}
		hdr := 8
		var vendor uint32
		if flags&FlagVendor != 0 {
			if length < 12 {
				return nil, fmt.Errorf("diameter: vendor AVP too short at %d", off)
			}
			vendor = binary.BigEndian.Uint32(b[off+8 : off+12])
			hdr = 12
		}
		data := b[off+hdr : off+length]
		avps = append(avps, AVP{Code: code, Flags: flags &^ FlagVendor, VendorID: vendor, Data: data})
		off += length + pad4(length)
	}
	return avps, nil
}

// ---- typed constructors ----

func mFlag(mandatory bool) byte {
	if mandatory {
		return FlagMandatory
	}
	return 0
}

// U32 builds an Unsigned32 AVP.
func U32(code, vendor uint32, mandatory bool, v uint32) AVP {
	d := make([]byte, 4)
	binary.BigEndian.PutUint32(d, v)
	return AVP{Code: code, VendorID: vendor, Flags: mFlag(mandatory), Data: d}
}

// I32 builds an Integer32 AVP.
func I32(code, vendor uint32, mandatory bool, v int32) AVP {
	return U32(code, vendor, mandatory, uint32(v))
}

// Str builds a UTF8String / OctetString AVP.
func Str(code, vendor uint32, mandatory bool, s string) AVP {
	return AVP{Code: code, VendorID: vendor, Flags: mFlag(mandatory), Data: []byte(s)}
}

// Octet builds an OctetString AVP from raw bytes.
func Octet(code, vendor uint32, mandatory bool, b []byte) AVP {
	return AVP{Code: code, VendorID: vendor, Flags: mFlag(mandatory), Data: b}
}

// Addr builds a Diameter Address AVP (RFC 6733 §4.3.1): 2-byte address family
// followed by the address. IPv4 => family 1, IPv6 => family 2.
func Addr(code, vendor uint32, mandatory bool, ip net.IP) AVP {
	var d []byte
	if v4 := ip.To4(); v4 != nil {
		d = append([]byte{0, 1}, v4...)
	} else {
		d = append([]byte{0, 2}, ip.To16()...)
	}
	return AVP{Code: code, VendorID: vendor, Flags: mFlag(mandatory), Data: d}
}

// FramedIP builds a Framed-IP-Address AVP in its RFC 7155 RADIUS-heritage form:
// raw 4-byte IPv4 with no address-family prefix — the encoding Kamailio's
// cdp_avp emits and expects. IPv6 is emitted as raw 16 bytes.
func FramedIP(code, vendor uint32, mandatory bool, ip net.IP) AVP {
	if v4 := ip.To4(); v4 != nil {
		return AVP{Code: code, VendorID: vendor, Flags: mFlag(mandatory), Data: v4}
	}
	return AVP{Code: code, VendorID: vendor, Flags: mFlag(mandatory), Data: ip.To16()}
}

// Grouped builds a grouped AVP from children.
func Grouped(code, vendor uint32, mandatory bool, children ...AVP) AVP {
	return AVP{Code: code, VendorID: vendor, Flags: mFlag(mandatory), Grouped: true, Group: children}
}

// ---- accessors ----

var errNotFound = errors.New("diameter: AVP not found")

// U32Val decodes the AVP data as Unsigned32.
func (a AVP) U32Val() (uint32, error) {
	if len(a.Data) != 4 {
		return 0, fmt.Errorf("diameter: AVP %d not 4 bytes", a.Code)
	}
	return binary.BigEndian.Uint32(a.Data), nil
}

// StrVal returns the AVP data as a string.
func (a AVP) StrVal() string { return string(a.Data) }

// IPVal decodes an address-carrying AVP to a net.IP. It accepts both the
// Diameter Address form (2-byte family prefix, RFC 6733 §4.3.1) and the raw
// RFC 7155 Framed-IP-Address forms (4-byte IPv4 / 16-byte IPv6) that
// Kamailio's cdp_avp sends. The forms are unambiguous by length.
func (a AVP) IPVal() (net.IP, error) {
	switch len(a.Data) {
	case 4, 16:
		return net.IP(a.Data), nil
	}
	if len(a.Data) < 6 {
		return nil, fmt.Errorf("diameter: address AVP %d too short", a.Code)
	}
	fam := binary.BigEndian.Uint16(a.Data[0:2])
	switch fam {
	case 1:
		return net.IP(a.Data[2:6]), nil
	case 2:
		if len(a.Data) < 18 {
			return nil, errors.New("diameter: IPv6 address AVP too short")
		}
		return net.IP(a.Data[2:18]), nil
	default:
		return nil, fmt.Errorf("diameter: unknown address family %d", fam)
	}
}

// SubAVPs decodes the grouped AVP's children on demand.
func (a AVP) SubAVPs() ([]AVP, error) {
	if a.Grouped {
		return a.Group, nil
	}
	return DecodeAVPs(a.Data)
}

// Find returns the first child AVP with the given (code, vendor) from a list.
func Find(avps []AVP, code, vendor uint32) (AVP, error) {
	for _, a := range avps {
		if a.Code == code && a.VendorID == vendor {
			return a, nil
		}
	}
	return AVP{}, errNotFound
}

// FindAll returns all child AVPs with the given (code, vendor).
func FindAll(avps []AVP, code, vendor uint32) []AVP {
	var out []AVP
	for _, a := range avps {
		if a.Code == code && a.VendorID == vendor {
			out = append(out, a)
		}
	}
	return out
}

// IsNotFound reports whether err is the "AVP not found" sentinel.
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }
