package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kenkeiter/plasticturtle/internal/config"
)

// A grant is one thing a .plasticturtle hands to the VM: the image it boots, a
// host directory it exposes, a port it opens, the egress it permits.
//
// Grants exist so that "what does this config give away" is computed in exactly
// one place. Both the approval summary and the change list that plasticturtle allow shows
// on re-approval are rendered from the same slice, so a config field can never
// be summarized but not diffed (or the reverse) — which would let a change slip
// past the user unmentioned.
type grant struct {
	kind grantKind
	// name identifies the grant within its kind: a mount name, a VM port, a
	// domain pattern. Empty for kinds that can only occur once. Two grants with
	// the same kind and name are the same grant, possibly with a changed value.
	name string
	// value is the thing granted; note is a qualifier shown beside it.
	value string
	note  string
}

// grantKind orders the change list and names each grant in it.
type grantKind int

const (
	grantImage grantKind = iota
	grantResources
	grantMount
	grantPort
	grantNetPolicy
	grantNetAllow
)

func (k grantKind) String() string {
	switch k {
	case grantImage:
		return "image"
	case grantResources:
		return "resources"
	case grantMount:
		return "mount"
	case grantPort:
		return "port"
	case grantNetPolicy:
		return "network"
	case grantNetAllow:
		return "allow"
	}
	return "unknown"
}

// label names the grant in the change list: "image", "mount data", "port 3000".
func (g grant) label() string {
	if g.name == "" {
		return g.kind.String()
	}
	return g.kind.String() + " " + g.name
}

// rendered is the whole grant on one line, for the change list. The summary
// lays value and note out in columns instead.
func (g grant) rendered() string {
	switch {
	case g.value == "":
		return g.note
	case g.note == "":
		return g.value
	}
	return g.value + " " + g.note
}

// grantsOf enumerates everything cfg grants a VM rooted at dir.
//
// It deliberately does not use config.Resolved: Resolve refuses a config whose
// mount source has been deleted, and a config that cannot boot still has to be
// describable — both to warn about it now and to diff a snapshot taken back
// when the directory existed. Paths are expanded the same way Resolve expands
// them, so the two agree on what a mount points at.
func grantsOf(cfg *config.Config, dir string) []grant {
	gs := []grant{
		{kind: grantImage, value: strings.TrimSpace(cfg.Image)},
		{kind: grantResources, value: describeResources(cfg.Resources)},
	}

	// The project mount is implicit and always present, so it is listed first
	// whether or not the file mentions it.
	gs = append(gs, grant{
		kind:  grantMount,
		name:  config.ProjectMountName,
		value: dir,
		note:  describeMode(projectMountMode(cfg)),
	})
	for _, m := range cfg.Mounts {
		if m.Name == config.ProjectMountName {
			continue // already emitted, with its mode applied
		}
		host, err := config.ExpandHostPath(m.HostPath, dir)
		if err != nil {
			// Show what the file literally asks for rather than dropping the
			// mount: a path pt cannot expand is still a path the user is being
			// asked to approve.
			host = m.HostPath
		}
		mode := m.Mode
		if mode == "" {
			mode = config.ModeRW
		}
		gs = append(gs, grant{kind: grantMount, name: m.Name, value: host, note: describeMode(mode)})
	}

	for _, p := range cfg.Ports {
		host := p.HostPort
		if host == 0 {
			host = p.VMPort
		}
		gs = append(gs, grant{
			kind:  grantPort,
			name:  fmt.Sprintf("VM %d", p.VMPort),
			value: fmt.Sprintf("host %d", host),
			note:  "(reachable on 127.0.0.1 only)",
		})
	}

	policy := config.NetOpen
	if cfg.Network != nil && cfg.Network.Policy != "" {
		policy = cfg.Network.Policy
	}
	if policy == config.NetRestricted {
		gs = append(gs, grant{kind: grantNetPolicy, value: string(policy), note: "(outbound denied except the domains below)"})
		for _, pat := range normalizedAllow(cfg.Network) {
			gs = append(gs, grant{kind: grantNetAllow, name: pat})
		}
	} else {
		gs = append(gs, grant{kind: grantNetPolicy, value: string(policy), note: "(unrestricted outbound)"})
	}
	return gs
}

// normalizedAllow returns the allowlist in the same canonical, deduplicated
// form the firewall will enforce. Diffing the raw strings instead would report
// a change for a config that only recased a domain.
func normalizedAllow(n *config.Network) []string {
	if n == nil {
		return nil
	}
	var out []string
	seen := make(map[string]struct{}, len(n.Allow))
	for _, pat := range n.Allow {
		norm, err := config.NormalizeDomainPattern(pat)
		if err != nil {
			// Validation rejects these before plasticturtle allow prompts; a snapshot from
			// an older pt might still contain one. Show it as written rather
			// than hiding a rule the user may have believed was in force.
			norm = pat
		}
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	return out
}

// change is one difference between two sets of grants.
type change struct {
	// op is '+' added, '-' removed, '~' same grant with a different value.
	op   byte
	kind grantKind
	// label and the renderings are precomputed so the renderer stays dumb.
	label string
	// unchanged is the part of a '~' grant that stayed the same — a mount's
	// path when only its mode moved. Printing it once ahead of the change beats
	// repeating a long path on both sides of the arrow, where the eye has to
	// compare the two to find the one word that differs.
	unchanged string
	old       string
	new       string
}

// diffGrants reports how new differs from old, in a stable order: by kind, then
// by the order the grants appear within their kind, which for mounts and ports
// is the order the file lists them.
//
// Identity is (kind, name), so renaming a mount reads as one removal and one
// addition — which is what it is, since the guest path changes too.
func diffGrants(old, cur []grant) []change {
	type slot struct {
		g   grant
		seq int
	}
	index := func(gs []grant) map[string]slot {
		m := make(map[string]slot, len(gs))
		for i, g := range gs {
			// A malformed config could repeat a name; validation rejects those,
			// and keeping the first is the conservative reading either way.
			if _, dup := m[g.label()]; dup {
				continue
			}
			m[g.label()] = slot{g: g, seq: i}
		}
		return m
	}
	oldByLabel, curByLabel := index(old), index(cur)

	var out []change
	for _, g := range cur {
		prev, existed := oldByLabel[g.label()]
		switch {
		case !existed:
			out = append(out, change{op: '+', kind: g.kind, label: g.label(), new: g.rendered()})
		case prev.g.value == g.value && prev.g.note != g.note:
			// Only the qualifier moved — a mount flipped between read-only and
			// read-write, say. That is exactly the change most worth reading,
			// so it gets the arrow to itself.
			out = append(out, change{op: '~', kind: g.kind, label: g.label(), unchanged: g.value, old: prev.g.note, new: g.note})
		case prev.g.note == g.note && prev.g.value != g.value:
			// The note is unchanged boilerplate here; repeating it on both
			// sides of the arrow would bury the one thing that moved.
			out = append(out, change{op: '~', kind: g.kind, label: g.label(), old: prev.g.value, new: g.value})
		case prev.g.rendered() != g.rendered():
			out = append(out, change{op: '~', kind: g.kind, label: g.label(), old: prev.g.rendered(), new: g.rendered()})
		}
	}
	for _, g := range old {
		if _, still := curByLabel[g.label()]; !still {
			// The note is dropped here: a withdrawn grant's qualifier ("this
			// port only listens on loopback") describes exposure the user no
			// longer has, and the line exists to say the grant is gone.
			out = append(out, change{op: '-', kind: g.kind, label: g.label(), old: g.value})
		}
	}

	// Sort by kind so related changes cluster, then by position in whichever
	// config the grant came from, so a file's own ordering survives.
	pos := func(c change) int {
		if s, ok := curByLabel[c.label]; ok {
			return s.seq
		}
		return oldByLabel[c.label].seq
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return pos(out[i]) < pos(out[j])
	})
	return out
}

// describeResources spells out a resource override, or says that the image
// decides. It takes a nil pointer because "absent" and "present but empty" mean
// the same thing here.
func describeResources(r *config.Resources) string {
	if r == nil || (r.CPU <= 0 && r.Memory <= 0) {
		return "inherited from the image"
	}
	var parts []string
	if r.CPU > 0 {
		parts = append(parts, fmt.Sprintf("%d vCPU", r.CPU))
	}
	if r.Memory > 0 {
		parts = append(parts, fmt.Sprintf("%d MiB memory", r.Memory))
	}
	return strings.Join(parts, ", ")
}

// describeMode spells out the mode, because "rw" understates what it grants.
func describeMode(m config.Mode) string {
	if m == config.ModeRO {
		return "read-only"
	}
	return "READ-WRITE"
}

func projectMountMode(c *config.Config) config.Mode {
	for _, m := range c.Mounts {
		if m.Name == config.ProjectMountName && m.Mode != "" {
			return m.Mode
		}
	}
	return config.ModeRW
}
