package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/trust"
)

// The supervisor refuses to boot a config the trust database does not know.
//
// pt shell already checks this, and anyone able to invoke `pt _supervise` can
// invoke `tart` directly — so this is layering, not a security boundary. It
// exists because _supervise accepts a whole config (image, mounts, modes) on
// stdin and acts on it, and "the only caller is well-behaved" is an assumption
// that stops being true quietly.
func TestBootRefusesAnUntrustedConfig(t *testing.T) {
	h := newHarness(t)

	// A trust database that knows nothing: exactly what a hand-invoked
	// _supervise, or one whose params were tampered with, would face.
	empty, err := trust.Open(h.store.Root + "/empty-trust.json")
	if err != nil {
		t.Fatal(err)
	}
	h.deps.Trust = empty

	h.writeCreating()
	h.start()
	err = h.finish()

	if err == nil {
		t.Fatal("the supervisor booted a config the trust database does not know")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("error = %v, want it to name the trust failure", err)
	}
	// Nothing may have been cloned: the refusal has to land before any VM
	// exists, or the check is merely advisory.
	for _, vm := range h.fake.Existing() {
		if vm == h.instance {
			t.Errorf("VM %s was cloned despite the config being untrusted", vm)
		}
	}
}

// A config allowed at a DIFFERENT hash is refused too: trust is over exact
// bytes, and a supervisor handed a stale or altered snapshot must not boot it.
func TestBootRefusesAConfigAllowedAtAnotherHash(t *testing.T) {
	h := newHarness(t)

	other, err := trust.Open(h.store.Root + "/other-trust.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Allow(h.projectDir, "sha256:"+strings.Repeat("b", 64), time.Now()); err != nil {
		t.Fatal(err)
	}
	h.deps.Trust = other

	h.writeCreating()
	h.start()

	if err := h.finish(); err == nil {
		t.Fatal("the supervisor booted a config allowed only at a different hash")
	}
}
