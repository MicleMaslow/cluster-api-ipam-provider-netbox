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

// Package layout places the pools and reserved addresses of a network in its prefix.
package layout

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Span is an inclusive address range.
type Span struct{ From, To netip.Addr }

// Pool is a pool of the spec.
type Pool struct {
	Name string
	Size int  // free addresses
	Rest bool // the rest instead of Size
}

// Group is a group of reserved addresses of the spec.
type Group struct {
	Name  string
	Count int
}

// PlacedPool is a pool placed in the prefix.
type PlacedPool struct {
	Pool
	Ranges []Span // ascending; the addresses between them are excluded
}

// PlacedGroup is a group placed in the prefix.
type PlacedGroup struct {
	Group
	Addrs []netip.Addr // from the top down; Addrs[i] has index i+1
}

// Layout is the placement of the pools and groups of a network.
type Layout struct {
	Pools  []PlacedPool
	Groups []PlacedGroup
}

// Space is what a layout is placed in.
type Space struct {
	Prefix, L2 netip.Prefix
	Gateway    netip.Addr
	Taken      []Span     // NetBox addresses and ranges that are not the network's
	VIP        netip.Addr // the reserved VIP, zero if none
	WantVIP    bool       // keep the highest free address for a VIP not reserved yet
	// Adopt are NetBox addresses without a key: a group placed for the first time takes the highest of them under the
	// VIP first, from the top down; those it does not take are taken.
	Adopt []netip.Addr
}

// mark marks in busy the addresses of Adopt and those of Taken inside the prefix.
func (s Space) mark(busy map[netip.Addr]bool) {
	for _, a := range s.Adopt {
		busy[a] = true
	}
	first := s.Prefix.Masked().Addr()
	for _, t := range s.Taken {
		for a := maxAddr(t.From, first); s.Prefix.Contains(a) && !t.To.Less(a); a = a.Next() {
			busy[a] = true
		}
	}
}

// free reports whether a is not busy and not the network or broadcast address of L2: the address after the
// broadcast is outside L2.
func (s Space) free(a netip.Addr, busy map[netip.Addr]bool) bool {
	return !busy[a] && a != s.L2.Masked().Addr() && s.L2.Contains(a.Next())
}

// grow adds a to the ascending runs rs.
func grow(rs []Span, a netip.Addr) []Span {
	if k := len(rs) - 1; k >= 0 && rs[k].To.Next() == a {
		rs[k].To = a
		return rs
	}
	return append(rs, Span{a, a})
}

// Runs returns the free runs of spans, ascending: the ranges of a pool restored from an InClusterIPPool.
func Runs(s Space, spans ...Span) []Span {
	busy := map[netip.Addr]bool{s.Gateway: true, s.VIP: true}
	s.mark(busy)
	spans = slices.Clone(spans)
	slices.SortFunc(spans, func(a, b Span) int { return a.From.Compare(b.From) })
	var out []Span
	for _, span := range spans {
		for a := span.From; a.IsValid() && !span.To.Less(a); a = a.Next() {
			if s.free(a, busy) {
				busy[a] = true // once, in overlapping spans
				out = grow(out, a)
			}
		}
	}
	return out
}

// Plan places pools from the start of the prefix and groups from its top, on free addresses.
// Leading pools equal to those of prev keep their ranges; groups keep their prev addresses, a group without them
// adopts the highest of s.Adopt.
func Plan(s Space, pools []Pool, groups []Group, prev Layout) (Layout, error) {
	if s.Prefix.Addr().BitLen()-s.Prefix.Bits() > 16 {
		return Layout{}, fmt.Errorf("%s: a layout needs a prefix of at most 65536 addresses", s.Prefix)
	}
	busy := map[netip.Addr]bool{s.Gateway: true, s.VIP: true}
	prevAddrs := map[string][]netip.Addr{}
	for _, g := range prev.Groups {
		prevAddrs[g.Name] = g.Addrs
	}
	kept := make([][]netip.Addr, len(groups))
	for i, g := range groups {
		a := prevAddrs[g.Name]
		kept[i] = slices.Clone(a[:min(g.Count, len(a))])
		for _, a := range kept[i] {
			busy[a] = true
		}
	}
	adopt := slices.DeleteFunc(slices.Clone(s.Adopt), func(a netip.Addr) bool {
		return !s.Prefix.Contains(a) || !s.free(a, busy) || s.VIP.IsValid() && !a.Less(s.VIP)
	})
	slices.SortFunc(adopt, func(a, b netip.Addr) int { return b.Compare(a) })
	for i, g := range groups {
		if len(kept[i]) == 0 {
			n := min(g.Count, len(adopt))
			kept[i], adopt = adopt[:n:n], adopt[n:]
		}
	}
	s.mark(busy)
	first := s.Prefix.Masked().Addr()
	var free []netip.Addr // ascending
	for a := first; s.Prefix.Contains(a); a = a.Next() {
		if s.free(a, busy) {
			free = append(free, a)
		}
	}

	var out Layout
	cursor := first
	for i, p := range pools {
		if i == len(prev.Pools) || p.Rest || prev.Pools[i].Pool != p || len(prev.Pools[i].Ranges) == 0 {
			break
		}
		out.Pools = append(out.Pools, prev.Pools[i])
		cursor = prev.Pools[i].Ranges[len(prev.Pools[i].Ranges)-1].To.Next()
	}
	start, _ := slices.BinarySearchFunc(free, cursor, netip.Addr.Compare)
	have, need := len(free)-start, 0
	var asks []string
	ask := func(what string, n int) {
		if n > 0 {
			need += n
			asks = append(asks, fmt.Sprintf("%s %d", what, n))
		}
	}
	rests := 0
	for _, p := range pools[len(out.Pools):] {
		if p.Rest {
			rests++
			ask(p.Name+" at least", 1)
		} else {
			ask(p.Name, p.Size)
		}
	}
	if rests > 1 {
		return Layout{}, fmt.Errorf("at most one pool of size rest, got %d", rests)
	}
	for i, g := range groups {
		ask(g.Name, g.Count-len(kept[i]))
	}
	vip := s.WantVIP && !s.VIP.IsValid()
	if vip {
		ask("VIP", 1)
	}
	if need > have {
		return Layout{}, fmt.Errorf("%s need %d free addresses, %d are free from %s", strings.Join(asks, ", "), need, have, cursor)
	}

	// Pools fill free from start up; the VIP and groups take the top, which need leaves to them.
	for _, p := range pools[len(out.Pools):] {
		n := p.Size
		if p.Rest {
			n = have - need + 1
		}
		pp := PlacedPool{Pool: p}
		for _, a := range free[start : start+n] {
			pp.Ranges = grow(pp.Ranges, a)
		}
		start += n
		out.Pools = append(out.Pools, pp)
	}
	top := len(free)
	if vip {
		top--
	}
	for i, g := range groups {
		pg := PlacedGroup{Group: g, Addrs: kept[i]}
		for len(pg.Addrs) < g.Count {
			top--
			pg.Addrs = append(pg.Addrs, free[top])
		}
		out.Groups = append(out.Groups, pg)
	}
	return out, nil
}

// Moved returns the allocated addresses whose pool in next is not the one in prev, and those next reserves for a group
// anew, ascending.
func Moved(prev, next Layout, allocated []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range allocated {
		if g := groupOf(next, a); poolOf(prev, a) != poolOf(next, a) || g != "" && g != groupOf(prev, a) {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return slices.Compact(out)
}

// poolOf returns the name of the pool whose ranges hold a, empty if none.
func poolOf(l Layout, a netip.Addr) string {
	for _, p := range l.Pools {
		for _, r := range p.Ranges {
			if !a.Less(r.From) && !r.To.Less(a) {
				return p.Name
			}
		}
	}
	return ""
}

// groupOf returns the name of the group that holds a, empty if none.
func groupOf(l Layout, a netip.Addr) string {
	for _, g := range l.Groups {
		if slices.Contains(g.Addrs, a) {
			return g.Name
		}
	}
	return ""
}

// Check compares a restored layout with the spec: names in order and sizes, rest takes any.
func Check(l Layout, pools []Pool) error {
	if slices.EqualFunc(l.Pools, pools, func(p PlacedPool, s Pool) bool {
		return p.Name == s.Name && (s.Rest || s.Size == p.Count())
	}) {
		return nil
	}
	found := make([]string, len(l.Pools))
	for i, p := range l.Pools {
		found[i] = fmt.Sprintf("%s %d", p.Name, p.Count())
	}
	return fmt.Errorf("sizes must be those found: %s", strings.Join(found, ", "))
}

func maxAddr(a, b netip.Addr) netip.Addr {
	if a.Less(b) {
		return b
	}
	return a
}

// Gaps returns the excluded addresses between the ranges.
func (p PlacedPool) Gaps() []Span {
	var gaps []Span
	for i := 1; i < len(p.Ranges); i++ {
		gaps = append(gaps, Span{p.Ranges[i-1].To.Next(), p.Ranges[i].From.Prev()})
	}
	return gaps
}

// Count returns the number of addresses in the ranges.
func (p PlacedPool) Count() int {
	n := 0
	for _, r := range p.Ranges {
		for a := r.From; a.IsValid() && !r.To.Less(a); a = a.Next() {
			n++
		}
	}
	return n
}
