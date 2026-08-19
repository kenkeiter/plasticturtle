package netfw

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// PolicyDoc is the on-disk hand-off between pt (which writes it from a resolved
// config) and the softnet shim (which reads it to build a Filter). It is a
// deliberately tiny, self-describing document: the shim runs in a security-
// sensitive spot and should parse as little as possible.
type PolicyDoc struct {
	// Policy is "open" or "restricted". Anything else, or absence, is treated by
	// the shim as open (pass-through) — a policy it cannot understand must not be
	// silently enforced as if it were restrictive, nor trusted to be permissive;
	// pt is responsible for writing a document the shim's version understands.
	Policy string `json:"policy"`
	// Allow is the normalized domain allowlist; meaningful only when restricted.
	Allow []string `json:"allow,omitempty"`
}

// ParsePolicy decodes a PolicyDoc. It rejects unknown fields so a typo in a
// security-relevant key is an error rather than a silently permissive policy.
func ParsePolicy(raw []byte) (*PolicyDoc, error) {
	var d PolicyDoc
	if err := strictUnmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("netfw: parse policy: %w", err)
	}
	return &d, nil
}

// Restricted reports whether the document asks for filtering.
func (d *PolicyDoc) Restricted() bool { return d != nil && d.Policy == "restricted" }

// Marshal renders the document as indented JSON with a trailing newline.
func (d *PolicyDoc) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func strictUnmarshal(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
