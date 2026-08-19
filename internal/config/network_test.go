package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateNetwork(t *testing.T) {
	base := func(n *Network) *Config {
		return &Config{Version: SchemaVersion, Image: "img", Network: n}
	}
	tests := []struct {
		name string
		net  *Network
		want string // "" = must pass; else a substring of the error
	}{
		{name: "absent is fine", net: nil},
		{name: "open no allow", net: &Network{Policy: NetOpen}},
		{name: "restricted empty allow denies all", net: &Network{Policy: NetRestricted}},
		{name: "restricted with domains", net: &Network{Policy: NetRestricted, Allow: []string{"github.com", "*.githubusercontent.com"}}},

		{name: "policy required when block present", net: &Network{Allow: []string{"github.com"}}, want: "network.policy: required"},
		{name: "policy unknown", net: &Network{Policy: "sometimes"}, want: `network.policy: must be`},
		{name: "allow under open rejected", net: &Network{Policy: NetOpen, Allow: []string{"github.com"}}, want: "network.allow: not permitted under policy"},

		{name: "bare tld rejected", net: &Network{Policy: NetRestricted, Allow: []string{"localhost"}}, want: "not a fully-qualified domain"},
		{name: "url rejected", net: &Network{Policy: NetRestricted, Allow: []string{"https://github.com"}}, want: "must be a bare domain"},
		{name: "host port rejected", net: &Network{Policy: NetRestricted, Allow: []string{"github.com:443"}}, want: "must be a bare domain"},
		{name: "interior wildcard rejected", net: &Network{Policy: NetRestricted, Allow: []string{"foo.*.com"}}, want: "leading label"},
		{name: "bare wildcard rejected", net: &Network{Policy: NetRestricted, Allow: []string{"*"}}, want: "parent domain"},
		{name: "wildcard tld rejected", net: &Network{Policy: NetRestricted, Allow: []string{"*.com"}}, want: "*.com"},
		{name: "empty entry rejected", net: &Network{Policy: NetRestricted, Allow: []string{""}}, want: "empty domain"},
		{name: "bad label rejected", net: &Network{Policy: NetRestricted, Allow: []string{"foo_bar.com"}}, want: "not a valid DNS label"},
		{name: "duplicate rejected", net: &Network{Policy: NetRestricted, Allow: []string{"github.com", "GitHub.com"}}, want: "duplicates network.allow[0]"},
		{name: "duplicate via trailing dot", net: &Network{Policy: NetRestricted, Allow: []string{"github.com", "github.com."}}, want: "duplicates"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := base(tt.net).Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("Validate() = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestNormalizeDomainPattern(t *testing.T) {
	tests := []struct {
		in, out string
		ok      bool
	}{
		{in: "github.com", out: "github.com", ok: true},
		{in: "  GitHub.COM  ", out: "github.com", ok: true},
		{in: "github.com.", out: "github.com", ok: true},
		{in: "*.githubusercontent.com", out: "*.githubusercontent.com", ok: true},
		{in: "a.b.c.example.com", out: "a.b.c.example.com", ok: true},
		{in: "xn--nxasmq6b.example", out: "xn--nxasmq6b.example", ok: true},
		{in: "", ok: false},
		{in: ".", ok: false},
		{in: "localhost", ok: false},
		{in: "*", ok: false},
		{in: "*.com", ok: false},
		{in: "foo.*.com", ok: false},
		{in: "10.0.0.1", ok: false}, // IP literal has no letters, but is a valid-looking label chain
	}
	for _, tt := range tests {
		got, err := normalizeDomainPattern(tt.in)
		if tt.ok && (err != nil || got != tt.out) {
			t.Errorf("normalizeDomainPattern(%q) = (%q, %v), want (%q, nil)", tt.in, got, err, tt.out)
		}
		if !tt.ok && err == nil {
			t.Errorf("normalizeDomainPattern(%q) = (%q, nil), want error", tt.in, got)
		}
	}
}

func TestResolveNetwork(t *testing.T) {
	tests := []struct {
		name string
		in   *Network
		want ResolvedNetwork
	}{
		{name: "nil defaults open", in: nil, want: ResolvedNetwork{Policy: NetOpen}},
		{name: "open drops allow", in: &Network{Policy: NetOpen}, want: ResolvedNetwork{Policy: NetOpen}},
		{
			name: "restricted normalizes and dedups",
			in:   &Network{Policy: NetRestricted, Allow: []string{"GitHub.com", "github.com.", "*.CDN.example."}},
			want: ResolvedNetwork{Policy: NetRestricted, Allow: []string{"github.com", "*.cdn.example"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveNetwork(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("resolveNetwork() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
