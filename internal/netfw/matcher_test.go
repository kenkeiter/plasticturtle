package netfw

import (
	"reflect"
	"testing"
)

func TestMatcher(t *testing.T) {
	m := NewMatcher([]string{"github.com", "*.githubusercontent.com", "Registry.NPMjs.org."})
	allow := []string{
		"github.com", "GitHub.com", "github.com.",
		"raw.githubusercontent.com", "a.b.githubusercontent.com",
		"registry.npmjs.org",
	}
	for _, h := range allow {
		if !m.Match(h) {
			t.Errorf("Match(%q) = false, want true", h)
		}
	}
	deny := []string{
		"", "evil.com", "notgithub.com", "github.com.evil.com",
		"githubusercontent.com", // wildcard parent is not itself matched
		"npmjs.org",             // exact rule was for registry.npmjs.org
		"github.io",
	}
	for _, h := range deny {
		if m.Match(h) {
			t.Errorf("Match(%q) = true, want false", h)
		}
	}
}

func TestMatcherIgnoresMalformedPatterns(t *testing.T) {
	m := NewMatcher([]string{"", "*", "*.", "foo.*.com", "good.com"})
	if !m.Match("good.com") {
		t.Fatal("good.com should match")
	}
	if m.Match("anything.com") {
		t.Fatal("malformed patterns must not widen the allowlist")
	}
	if got, want := m.Patterns(), []string{"good.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Patterns() = %v, want %v", got, want)
	}
}

func TestMatcherEmpty(t *testing.T) {
	if !NewMatcher(nil).Empty() {
		t.Fatal("nil allowlist should be Empty")
	}
	if NewMatcher([]string{"x.com"}).Empty() {
		t.Fatal("non-empty allowlist should not be Empty")
	}
}
