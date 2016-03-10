// Copyright (2015) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"errors"
	"fmt"
	"minicli"
	log "minilog"
	"strconv"
	"strings"
	"sync"
)

const BlacklistedVLAN = "BLACKLISTED"
const VLANAliasSep = "//"
const VLANStart, VLANEnd = 2, 4095

type Range struct {
	min, max, next int
}

// AllocatedVLANs stores the state for the VLANs that we've allocated so far
type AllocatedVLANs struct {
	byVLAN  map[int]string
	byAlias map[string]int

	ranges map[string]*Range

	sync.Mutex
}

var allocatedVLANs = NewAllocatedVLANs()

func NewAllocatedVLANs() *AllocatedVLANs {
	return &AllocatedVLANs{
		byVLAN:  make(map[int]string),
		byAlias: make(map[string]int),
		ranges: map[string]*Range{
			"": &Range{
				min:  VLANStart,
				max:  VLANEnd,
				next: VLANStart,
			},
		},
	}
}

// broadcastUpdate sends out the updated VLAN mapping to all the nodes so that
// if the head node crashes we can recover which VLANs map to which aliases.
func (v *AllocatedVLANs) broadcastUpdate(alias string, vlan int) {
	cmd := minicli.MustCompilef("vlans add %v %v", alias, vlan)
	respChan := make(chan minicli.Responses)

	go func() {
		for resps := range respChan {
			for _, resp := range resps {
				if resp.Error != "" {
					log.Debug("unable to send alias %v -> %v to %v: %v", alias, vlan, resp.Host, resp.Error)
				}
			}
		}
	}()
	go meshageSend(cmd, Wildcard, respChan)
}

// GetOrAllocate looks up the VLAN for the provided alias. If one has not
// already been assigned, it will allocate the next available VLAN.
func (v *AllocatedVLANs) GetOrAllocate(alias string) int {
	if vlan, ok := v.byAlias[alias]; ok {
		return vlan
	}

	// Not assigned, find the next VLAN
	v.Lock()
	defer v.Unlock()

	// Find the next unallocated VLAN, taking into account that a range may be
	// specified for the supplied alias.
	r := v.ranges[""] // default
	for prefix, r2 := range v.ranges {
		if strings.HasPrefix(alias, prefix+VLANAliasSep) {
			r = r2
		}
	}

	// Find the next unallocated VLAN
outer:
	for {
		// Look to see if a VLAN is already allocated
		for v.byVLAN[r.next] != "" {
			r.next += 1
		}

		// Ensure that we're within the specified bounds
		if r.next > r.max {
			// Ran out of VLANs... what is the right behavior?
			log.Fatal("ran out of VLANs")
		}

		// If we're in the default range, make sure we don't allocate anything
		// in a reserved range of VLANs
		if r == v.ranges[""] {
			for prefix, r2 := range v.ranges {
				if prefix == "" {
					continue
				}

				if r.next >= r2.min && r.next <= r2.max {
					r.next = r.max + 1
					continue outer
				}
			}
		}

		// all the checks passed
		break
	}

	log.Debug("adding VLAN alias %v => %v", alias, r.next)

	v.byVLAN[r.next] = alias
	v.byAlias[alias] = r.next

	v.broadcastUpdate(alias, r.next)

	return r.next
}

// AddAlias sets the VLAN for the provided alias.
func (v *AllocatedVLANs) AddAlias(alias string, vlan int) error {
	v.Lock()
	defer v.Unlock()

	log.Debug("adding VLAN alias %v => %v", alias, vlan)

	if _, ok := v.byAlias[alias]; ok {
		return errors.New("alias already in use")
	}
	if _, ok := v.byVLAN[vlan]; ok {
		return errors.New("vlan already in use")
	}

	v.byVLAN[vlan] = alias
	v.byAlias[alias] = vlan

	return nil
}

// GetVLAN returns the alias for a given VLAN or DisconnectedVLAN if it has not
// been assigned an alias.
func (v *AllocatedVLANs) GetVLAN(alias string) int {
	v.Lock()
	defer v.Unlock()

	if vlan, ok := v.byAlias[alias]; ok {
		return vlan
	}

	return DisconnectedVLAN
}

// GetAlias returns the alias for a given VLAN or the empty string if it has
// not been assigned an alias. Note that previously Blacklist'ed VLANs will
// return the const BlacklistedVLAN.
func (v *AllocatedVLANs) GetAlias(vlan int) string {
	v.Lock()
	defer v.Unlock()

	return v.byVLAN[vlan]
}

// Delete allocation for aliases matching a given prefix.
func (v *AllocatedVLANs) Delete(prefix string) {
	v.Lock()
	defer v.Unlock()

	log.Debug("deleting VLAN aliases with prefix: `%v`", prefix)

	for alias, vlan := range v.byAlias {
		if strings.HasPrefix(alias, prefix) {
			delete(v.byVLAN, vlan)
			delete(v.byAlias, alias)
		}
	}

	if prefix != "" {
		delete(v.ranges, strings.TrimSuffix(prefix, VLANAliasSep))
	}

	// Reset next counter so that we can find the recently freed VLANs
	for _, r := range v.ranges {
		r.next = r.min
	}
}

// SetRange reserves a range of VLANs for a particular prefix.
func (v *AllocatedVLANs) SetRange(prefix string, min, max int) error {
	v.Lock()
	defer v.Unlock()

	// Test for conflicts with other ranges
	for prefix2, r := range v.ranges {
		if prefix == prefix2 || prefix2 == "" {
			continue
		}

		if min <= r.max && r.min <= max {
			return fmt.Errorf("range overlaps with another namespace: %v", prefix2)
		}
	}

	// Warn if we detect any holes in the range
	for i := min; i <= max; i++ {
		if _, ok := v.byVLAN[i]; ok {
			log.Warn("detected hole in VLAN range %v -> %v: %v", min, max, i)
		}
	}

	v.ranges[prefix] = &Range{
		min:  min,
		max:  max,
		next: min,
	}

	return nil
}

// Blacklist marks a VLAN as manually configured which removes it from the
// allocation pool. For instance, if a user runs `vm config net 100`, VLAN 100
// would be marked as blacklisted.
func (v *AllocatedVLANs) Blacklist(vlan int) {
	v.Lock()
	defer v.Unlock()

	if alias, ok := v.byVLAN[vlan]; ok {
		delete(v.byAlias, alias)
	}
	v.byVLAN[vlan] = BlacklistedVLAN
}

// PrintVLAN prints the alias for the VLAN, if one is set. Will trim off the
// namespace prefix if it matches the currently active namespace.
func (v *AllocatedVLANs) PrintVLAN(vlan int) string {
	v.Lock()
	defer v.Unlock()

	if alias, ok := v.byVLAN[vlan]; ok && alias != BlacklistedVLAN {
		// If we're in the namespace identified by the alias, we can trim off
		// the `<namespace>//` prefix.
		parts := strings.Split(alias, VLANAliasSep)
		if namespace == parts[0] {
			alias = strings.Join(parts[1:], VLANAliasSep)
		}

		return fmt.Sprintf("%v (%d)", alias, vlan)
	}

	return strconv.Itoa(vlan)
}
