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

package netmath

import (
	"net/netip"
	"testing"
)

func TestAddressAt(t *testing.T) {
	p := netip.MustParsePrefix("10.10.1.0/26")
	for _, tc := range []struct {
		offset int
		want   string
	}{
		{0, "10.10.1.0"}, {14, "10.10.1.14"}, {63, "10.10.1.63"}, {-1, "10.10.1.63"}, {-64, "10.10.1.0"},
	} {
		got, err := AddressAt(p, tc.offset)
		if err != nil || got.String() != tc.want {
			t.Errorf("AddressAt(%s, %d) = %s, %v; want %s", p, tc.offset, got, err, tc.want)
		}
	}
	for _, offset := range []int{64, -65} {
		if _, err := AddressAt(p, offset); err == nil {
			t.Errorf("AddressAt(%s, %d): want error", p, offset)
		}
	}
	if got, _ := AddressAt(netip.MustParsePrefix("10.10.1.5/26"), 0); got.String() != "10.10.1.0" {
		t.Errorf("unmasked prefix: got %s", got)
	}
	if _, err := AddressAt(netip.MustParsePrefix("2001:db8::/64"), 0); err == nil {
		t.Error("IPv6: want error")
	}
}

func TestLastAndCount(t *testing.T) {
	if got := Last(netip.MustParsePrefix("10.10.0.0/22")); got.String() != "10.10.3.255" {
		t.Errorf("Last = %s", got)
	}
	if got := Count(netip.MustParseAddr("10.10.0.14"), netip.MustParseAddr("10.10.0.16")); got != 3 {
		t.Errorf("Count = %d", got)
	}
}

func TestContainsAndCover(t *testing.T) {
	a := netip.MustParseAddr
	if !Contains(a("10.10.0.66"), a("10.10.0.77"), a("10.10.0.66")) || Contains(a("10.10.0.66"), a("10.10.0.77"), a("10.10.0.78")) {
		t.Error("Contains")
	}
	for _, tc := range []struct{ start, end, want string }{
		{"10.10.0.66", "10.10.0.77", "10.10.0.64/28"},
		{"10.10.0.66", "10.10.0.66", "10.10.0.66/32"},
		{"10.10.0.254", "10.10.1.1", "10.10.0.0/23"},
	} {
		if got := Cover(a(tc.start), a(tc.end)); got.String() != tc.want {
			t.Errorf("Cover(%s, %s) = %s, want %s", tc.start, tc.end, got, tc.want)
		}
	}
}
