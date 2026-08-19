// Package netfw is the host-side domain firewall that Plastic Turtle's softnet
// shim applies to a guest's network frames.
//
// The enforcement model is DNS-pinning. The guest's DNS still resolves through
// the vmnet gateway, and every response passes back through the shim. For a name
// the policy allows, the shim records the answer's IP addresses in a
// short-lived pin table; every other outbound frame is permitted only if its
// destination is a pinned address. A tool that hardcodes an IP, resolves out of
// band, or speaks IPv6 therefore reaches nothing, because its target was never
// pinned — the firewall fails closed. Names the policy does not allow are
// answered with NXDOMAIN so the guest fails fast and legibly rather than hanging
// on a connect timeout.
//
// This package is pure and clock-injected: it parses and rules on byte slices
// and holds no sockets, so the whole verdict path is unit-testable without a VM.
package netfw

import (
	"sort"
	"strings"
)

// Matcher decides whether a hostname is permitted by an allowlist. It is built
// once from a resolved policy and is read-only (and safe for concurrent use)
// thereafter.
type Matcher struct {
	exact    map[string]struct{} // "github.com"
	wildcard map[string]struct{} // parent domain of "*.x": "githubusercontent.com"
}

// NewMatcher compiles allow patterns into a Matcher. Patterns are expected in
// the normalized form config.ResolveNetwork emits: lowercase, no trailing dot,
// either "domain" or "*.domain". Anything else is ignored rather than trusted —
// a malformed pattern must never widen the allowlist.
func NewMatcher(patterns []string) *Matcher {
	m := &Matcher{
		exact:    make(map[string]struct{}),
		wildcard: make(map[string]struct{}),
	}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(p), "."))
		switch {
		case p == "":
			continue
		case strings.HasPrefix(p, "*."):
			parent := p[2:]
			if parent != "" && !strings.Contains(parent, "*") {
				m.wildcard[parent] = struct{}{}
			}
		case !strings.Contains(p, "*"):
			m.exact[p] = struct{}{}
		}
	}
	return m
}

// Match reports whether host is permitted. An exact pattern matches only itself;
// a "*.parent" pattern matches any proper subdomain of parent (a.parent,
// a.b.parent) but not the bare parent. Matching is case-insensitive and ignores
// a trailing dot.
//
// Note the asymmetry with exact rules: "*.example.com" does NOT admit
// "example.com". A user who wants both lists both. This mirrors how browsers and
// cookie scopes treat wildcards and avoids a wildcard silently granting its
// parent.
func (m *Matcher) Match(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	if _, ok := m.exact[host]; ok {
		return true
	}
	// Walk each parent suffix: for "a.b.example.com" test "b.example.com",
	// "example.com", "com" against the wildcard set. The first dot is stripped
	// so the bare name itself is never treated as its own wildcard parent.
	rest := host
	for {
		dot := strings.IndexByte(rest, '.')
		if dot < 0 {
			return false
		}
		rest = rest[dot+1:]
		if _, ok := m.wildcard[rest]; ok {
			return true
		}
	}
}

// Patterns returns the compiled patterns in a stable, human-readable order, for
// logging and tests. Exact names and "*." wildcards are interleaved
// lexicographically by their matched domain.
func (m *Matcher) Patterns() []string {
	out := make([]string, 0, len(m.exact)+len(m.wildcard))
	for d := range m.exact {
		out = append(out, d)
	}
	for d := range m.wildcard {
		out = append(out, "*."+d)
	}
	sort.Strings(out)
	return out
}

// Empty reports whether the allowlist admits nothing. A restricted policy with
// an empty matcher denies all egress, which is legal but worth surfacing.
func (m *Matcher) Empty() bool { return len(m.exact) == 0 && len(m.wildcard) == 0 }
