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

package layout

import (
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func ip(n int) netip.Addr { return netip.AddrFrom4([4]byte{10, 10, 0, byte(n)}) }

func r(from, to int) Span { return Span{ip(from), ip(to)} }

func placed(name string, size int, ranges ...Span) PlacedPool {
	return PlacedPool{Pool{Name: name, Size: size}, ranges}
}

func rest(name string, ranges ...Span) PlacedPool {
	return PlacedPool{Pool{Name: name, Rest: true}, ranges}
}

func group(name string, addrs ...int) PlacedGroup {
	g := PlacedGroup{Group: Group{Name: name, Count: len(addrs)}}
	for _, n := range addrs {
		g.Addrs = append(g.Addrs, ip(n))
	}
	return g
}

func pools(pp ...PlacedPool) []Pool {
	var out []Pool
	for _, p := range pp {
		out = append(out, p.Pool)
	}
	return out
}

func metallb(n int) []Group { return []Group{{Name: "metallb", Count: n}} }

var (
	base  = []Pool{{Name: "cp", Size: 6}, {Name: "ingress", Size: 8}, {Name: "workers", Rest: true}}
	fresh = Layout{
		Pools:  []PlacedPool{placed("cp", 6, r(2, 7)), placed("ingress", 8, r(8, 15)), rest("workers", r(16, 60))},
		Groups: []PlacedGroup{group("metallb", 62, 61)},
	}
	kept = Layout{
		Pools:  []PlacedPool{placed("cp", 6, r(2, 7)), placed("ingress", 8, r(8, 15)), rest("workers", r(16, 59))},
		Groups: []PlacedGroup{group("metallb", 62, 61, 60)},
	}
)

func TestPlan(t *testing.T) {
	big := []Pool{{Name: "cp", Size: 70}, {Name: "ingress", Size: 8}, {Name: "workers", Rest: true}}
	twenty := Layout{Pools: []PlacedPool{placed("cp", 6, r(2, 7)), placed("ingress", 8, r(8, 15)), placed("workers", 20, r(16, 35))}}
	withExtra := Layout{
		Pools:  append(slices.Clone(twenty.Pools), placed("extra", 4, r(36, 39))),
		Groups: []PlacedGroup{group("metallb", 62, 61)},
	}
	noVIP := func(top int, addrs ...int) Layout {
		return Layout{
			Pools:  []PlacedPool{placed("cp", 6, r(2, 7)), placed("ingress", 8, r(8, 15)), rest("workers", r(16, top))},
			Groups: []PlacedGroup{group("metallb", addrs...)},
		}
	}
	middle := Layout{Pools: []PlacedPool{placed("cp", 6, r(2, 7)), rest("workers", r(8, 54)), placed("ingress", 8, r(55, 62))}}
	for _, tc := range []struct {
		name    string
		pools   []Pool
		groups  []Group
		taken   []Span
		wantVIP bool // no VIP reserved yet
		prev    Layout
		want    Layout
		err     string
	}{
		{name: "fresh", pools: base, groups: metallb(2), want: fresh},
		{name: "taken-inside", pools: base, groups: metallb(2), taken: []Span{r(10, 10)}, want: Layout{
			Pools:  []PlacedPool{placed("cp", 6, r(2, 7)), placed("ingress", 8, r(8, 9), r(11, 16)), rest("workers", r(17, 60))},
			Groups: []PlacedGroup{group("metallb", 62, 61)},
		}},
		{name: "want-vip", pools: base, groups: metallb(2), wantVIP: true, want: fresh},
		{name: "vip-under-kept-groups", pools: base, groups: metallb(2), wantVIP: true, prev: noVIP(61, 63, 62), want: noVIP(60, 63, 62)},
		{name: "keeps-unchanged-pools", pools: base, groups: metallb(3), taken: []Span{r(5, 5)}, prev: fresh, want: kept},
		{name: "count-down", pools: base, groups: metallb(2), taken: []Span{r(5, 5)}, prev: kept, want: fresh},
		{name: "rest-in-the-middle", pools: pools(middle.Pools...), want: middle},
		{name: "new-pool-at-the-end", pools: pools(withExtra.Pools...), groups: metallb(2), prev: twenty, want: withExtra},
		{name: "does-not-fit", pools: big, groups: metallb(2), err: "cp 70"},
		{name: "rest-zero", pools: append([]Pool{{Name: "cp", Size: 59}}, base[1:]...), groups: metallb(2), err: "workers at least 1"},
		{name: "rest-exactly-zero", pools: append([]Pool{{Name: "cp", Size: 51}}, base[1:]...), groups: metallb(2), err: "need 62 free addresses, 61 are free"},
		{name: "two-rests", pools: append(slices.Clone(base), Pool{Name: "extra", Rest: true}), err: "at most one pool of size rest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Space{
				Prefix:  netip.MustParsePrefix("10.10.0.0/26"),
				L2:      netip.MustParsePrefix("10.10.0.0/22"),
				Gateway: ip(1),
				Taken:   tc.taken,
				VIP:     ip(63),
			}
			if tc.wantVIP {
				s.VIP, s.WantVIP = netip.Addr{}, true
			}
			got, err := Plan(s, tc.pools, tc.groups, tc.prev)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Plan = %v, %v\nwant %v", got, err, tc.want)
			}
		})
	}
}

// space is the Space of TestPlan.
func space(taken ...Span) Space {
	return Space{Prefix: netip.MustParsePrefix("10.10.0.0/26"), L2: netip.MustParsePrefix("10.10.0.0/22"), Gateway: ip(1), Taken: taken, VIP: ip(63)}
}

func TestPlanAdoptsTail(t *testing.T) {
	s := space()
	s.Adopt = []netip.Addr{ip(58), ip(62), ip(61)}
	want := Layout{
		Pools:  []PlacedPool{placed("cp", 6, r(2, 7)), placed("ingress", 8, r(8, 15)), rest("workers", r(16, 57), r(59, 60))},
		Groups: []PlacedGroup{group("metallb", 62, 61)},
	}
	if got, err := Plan(s, base, metallb(2), Layout{}); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Plan = %v, %v\nwant %v", got, err, want)
	}
	want.Groups = []PlacedGroup{group("metallb", 62, 61, 58)}
	if got, err := Plan(s, base, metallb(3), Layout{}); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("all adopted: Plan = %v, %v\nwant %v", got, err, want)
	}
	// Groups adopt under the VIP only.
	s.VIP, s.Adopt = ip(61), []netip.Addr{ip(62), ip(60)}
	if got, err := Plan(s, base, metallb(1), Layout{}); err != nil || !reflect.DeepEqual(got.Groups, []PlacedGroup{group("metallb", 60)}) {
		t.Errorf("under the VIP: Plan = %v, %v", got, err)
	}
	s.VIP = ip(63)
	// A group with addresses of its own adopts nothing.
	s.Adopt = []netip.Addr{ip(58)}
	if got, err := Plan(s, base, metallb(2), fresh); err != nil || !reflect.DeepEqual(got.Groups, fresh.Groups) {
		t.Errorf("kept: Plan = %v, %v", got, err)
	}
}

func TestRuns(t *testing.T) {
	if got, want := Runs(space(r(20, 20)), r(16, 35)), []Span{r(16, 19), r(21, 35)}; !slices.Equal(got, want) {
		t.Errorf("Runs = %v, want %v", got, want)
	}
	s := space()
	s.Adopt = []netip.Addr{ip(3)}
	if got, want := Runs(s, r(0, 5)), []Span{r(2, 2), r(4, 5)}; !slices.Equal(got, want) {
		t.Errorf("Runs = %v, want %v", got, want)
	}
	// Spans in any order, adjacent or overlapping, give one ascending list.
	if got, want := Runs(space(r(20, 20)), r(30, 35), r(16, 25), r(26, 27), r(24, 26)), []Span{r(16, 19), r(21, 27), r(30, 35)}; !slices.Equal(got, want) {
		t.Errorf("Runs = %v, want %v", got, want)
	}
}

func TestPlanRefusesLargePrefix(t *testing.T) {
	s := Space{Prefix: netip.MustParsePrefix("10.0.0.0/15"), L2: netip.MustParsePrefix("10.0.0.0/8")}
	if _, err := Plan(s, base, nil, Layout{}); err == nil {
		t.Error("want error")
	}
}

func TestMoved(t *testing.T) {
	cp7 := Layout{Pools: []PlacedPool{placed("cp", 7, r(2, 8)), placed("ingress", 8, r(9, 16)), rest("workers", r(17, 60))}}
	for _, tc := range []struct {
		name      string
		next      Layout
		allocated []int
		want      []netip.Addr
	}{
		{"pool-shrunk", kept, []int{20, 60, 3}, []netip.Addr{ip(60)}},
		{"pool-shifted", cp7, []int{8}, []netip.Addr{ip(8)}},
		{"ascending", cp7, []int{16, 8, 16, 3}, []netip.Addr{ip(8), ip(16)}},
		{"reserved", Layout{Pools: fresh.Pools, Groups: []PlacedGroup{group("metallb", 62, 61, 63)}}, []int{63, 62}, []netip.Addr{ip(63)}},
		{"other-group", Layout{Pools: fresh.Pools, Groups: []PlacedGroup{group("metallb", 62), group("vrrp", 61)}}, []int{61, 62}, []netip.Addr{ip(61)}},
		{"released", Layout{Pools: fresh.Pools, Groups: []PlacedGroup{group("metallb", 62)}}, []int{61}, nil},
	} {
		var allocated []netip.Addr
		for _, n := range tc.allocated {
			allocated = append(allocated, ip(n))
		}
		if got := Moved(fresh, tc.next, allocated); !slices.Equal(got, tc.want) {
			t.Errorf("%s: Moved = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCheck(t *testing.T) {
	found := Layout{Pools: []PlacedPool{placed("cp", 0, r(2, 7)), placed("workers", 0, r(8, 20), r(22, 25))}}
	if err := Check(found, []Pool{{Name: "cp", Size: 6}, {Name: "workers", Rest: true}}); err != nil {
		t.Errorf("rest: %v", err)
	}
	err := Check(found, []Pool{{Name: "cp", Size: 6}, {Name: "workers", Size: 18}})
	if err == nil || err.Error() != "sizes must be those found: cp 6, workers 17" {
		t.Errorf("size: %v", err)
	}
	if err := Check(found, []Pool{{Name: "workers", Rest: true}, {Name: "cp", Size: 6}}); err == nil {
		t.Error("order: want error")
	}
	if err := Check(found, []Pool{{Name: "cp", Size: 6}}); err == nil {
		t.Error("fewer pools: want error")
	}
}

func TestGapsAndCount(t *testing.T) {
	p := placed("workers", 0, r(2, 4), r(6, 9), r(12, 12))
	if got, want := p.Gaps(), []Span{r(5, 5), r(10, 11)}; !slices.Equal(got, want) {
		t.Errorf("Gaps = %v, want %v", got, want)
	}
	if got := p.Count(); got != 8 {
		t.Errorf("Count = %d", got)
	}
}
