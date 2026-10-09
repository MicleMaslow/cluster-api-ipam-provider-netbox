/*
Copyright 2026 The cluster-api-ipam-provider-netbox Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package netmath is the address arithmetic pools are described in.
package netmath

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// AddressAt returns the address at offset in p; a negative offset counts back from the last address.
// ponytail: IPv4 only, add IPv6 when a dual-stack cluster appears.
func AddressAt(p netip.Prefix, offset int) (netip.Addr, error) {
	p = p.Masked()
	if !p.Addr().Is4() {
		return netip.Addr{}, fmt.Errorf("%s: only IPv4 is supported", p)
	}
	size := 1 << (32 - p.Bits())
	i := offset
	if i < 0 {
		i += size
	}
	if i < 0 || i >= size {
		return netip.Addr{}, fmt.Errorf("offset %d is outside %s", offset, p)
	}
	b := p.Addr().As4()
	binary.BigEndian.PutUint32(b[:], binary.BigEndian.Uint32(b[:])+uint32(i))
	return netip.AddrFrom4(b), nil
}

// Last returns the last address of p.
func Last(p netip.Prefix) netip.Addr {
	a, _ := AddressAt(p, -1)
	return a
}

// Count returns how many addresses lie in [start, end].
func Count(start, end netip.Addr) int {
	s, e := start.As4(), end.As4()
	return int(binary.BigEndian.Uint32(e[:])-binary.BigEndian.Uint32(s[:])) + 1
}

// Contains reports whether a lies in [start, end].
func Contains(start, end, a netip.Addr) bool {
	return !a.Less(start) && !end.Less(a)
}

// Cover returns the smallest prefix that holds start and end.
func Cover(start, end netip.Addr) netip.Prefix {
	for bits := start.BitLen(); bits >= 0; bits-- {
		if p, err := start.Prefix(bits); err == nil && p.Contains(end) {
			return p
		}
	}
	return netip.Prefix{}
}
