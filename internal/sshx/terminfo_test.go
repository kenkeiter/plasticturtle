package sshx

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// hostKnows reports whether this machine's terminfo database can resolve term.
// The negotiation's install path reads the host's own description, so a test
// that exercises it has to name a terminal the host actually has.
func hostKnows(t *testing.T, term string) bool {
	t.Helper()
	return exec.Command("infocmp", term).Run() == nil
}

// fakeGuest models a guest terminfo database for the negotiation: it answers
// the probe from a set of known names, and tic adds to that set.
type fakeGuest struct {
	mu sync.Mutex
	// known is what the guest resolves now; pending is what a successful tic
	// would add to it.
	known    map[string]bool
	pending  map[string]bool
	installs int
	stdin    []byte
	// ticFails makes the install fail the way a guest without tic, or without a
	// writable home, would.
	ticFails bool
	// ticLies makes tic succeed without the entry becoming resolvable, which is
	// what an entry whose header declares different names does.
	ticLies bool
}

func (g *fakeGuest) handle(cmd string, stdin io.Reader) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	if cmd == terminfoInstallCommand {
		g.installs++
		g.stdin, _ = io.ReadAll(stdin)
		if g.ticFails {
			return 1
		}
		if !g.ticLies {
			for name := range g.pending {
				g.known[name] = true
			}
		}
		return 0
	}
	// The probe is `infocmp '<term>' >/dev/null 2>&1`; recover the name from it
	// rather than re-deriving the quoting rules here.
	term := strings.TrimSuffix(strings.TrimPrefix(cmd, "infocmp '"), `' >/dev/null 2>&1`)
	if g.known[term] {
		return 0
	}
	return 1
}

// init seeds the names the guest resolves already, and the ones it would learn
// from a successful install.
func (g *fakeGuest) init(known []string, learns []string) *fakeGuest {
	g.known = map[string]bool{}
	for _, k := range known {
		g.known[k] = true
	}
	g.pending = map[string]bool{}
	for _, l := range learns {
		g.pending[l] = true
	}
	return g
}

// TestNegotiateTermKeepsANameTheGuestKnows is the cheap path: no install, no
// fallback, the user's own terminal name reaches the PTY.
func TestNegotiateTermKeepsANameTheGuestKnows(t *testing.T) {
	t.Parallel()

	srv, c := dialTestServer(t)
	g := (&fakeGuest{}).init([]string{"xterm-kitty"}, nil)
	srv.setTerminfoHandler(g.handle)

	if got := c.negotiateTerm(context.Background(), "xterm-kitty"); got != "xterm-kitty" {
		t.Fatalf("negotiateTerm = %q, want xterm-kitty", got)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.installs != 0 {
		t.Fatalf("installed terminfo %d times into a guest that already knew the term", g.installs)
	}
}

// TestNegotiateTermInstallsMissingEntry is the fix for the reported bug: a
// terminal whose entry ships only on the host must be taught to the guest, and
// the session must then still request the real name.
func TestNegotiateTermInstallsMissingEntry(t *testing.T) {
	t.Parallel()

	// vt100 stands in for xterm-ghostty: the test needs a name this host can
	// describe, and the negotiation cannot tell the two cases apart.
	const term = "vt100"
	if !hostKnows(t, term) {
		t.Skipf("host terminfo has no %s to install", term)
	}

	srv, c := dialTestServer(t)
	g := (&fakeGuest{}).init(nil, []string{term})
	srv.setTerminfoHandler(g.handle)

	if got := c.negotiateTerm(context.Background(), term); got != term {
		t.Fatalf("negotiateTerm = %q, want %q", got, term)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.installs != 1 {
		t.Fatalf("terminfo installs = %d, want 1", g.installs)
	}
	// What crossed the wire has to be something tic could actually compile.
	if !strings.Contains(string(g.stdin), term) || !strings.Contains(string(g.stdin), ",") {
		t.Fatalf("guest was sent no usable description: %q", g.stdin)
	}
}

// TestNegotiateTermFallsBackWhenInstallFails covers a guest with no tic, or no
// writable home. A plain TERM is a degraded session; a TERM the guest cannot
// resolve is a broken one, and the fallback is what keeps the difference.
func TestNegotiateTermFallsBackWhenInstallFails(t *testing.T) {
	t.Parallel()

	const term = "vt100"
	if !hostKnows(t, term) {
		t.Skipf("host terminfo has no %s to install", term)
	}

	srv, c := dialTestServer(t)
	g := (&fakeGuest{ticFails: true}).init(nil, []string{term})
	srv.setTerminfoHandler(g.handle)

	if got := c.negotiateTerm(context.Background(), term); got != defaultTerm {
		t.Fatalf("negotiateTerm = %q, want the %s fallback", got, defaultTerm)
	}
}

// TestNegotiateTermFallsBackWhenTicDoesNotTakeEffect is why the install is
// verified rather than trusted: tic reports success for a description stored
// under names that need not include the one we asked about.
func TestNegotiateTermFallsBackWhenTicDoesNotTakeEffect(t *testing.T) {
	t.Parallel()

	const term = "vt100"
	if !hostKnows(t, term) {
		t.Skipf("host terminfo has no %s to install", term)
	}

	srv, c := dialTestServer(t)
	g := (&fakeGuest{ticLies: true}).init(nil, []string{term})
	srv.setTerminfoHandler(g.handle)

	if got := c.negotiateTerm(context.Background(), term); got != defaultTerm {
		t.Fatalf("negotiateTerm = %q, want the %s fallback", got, defaultTerm)
	}
}

// TestNegotiateTermFallsBackWhenHostCannotDescribeIt covers a $TERM this host
// has no entry for either — there is nothing to install, so there is nothing to
// do but send a name that works.
func TestNegotiateTermFallsBackWhenHostCannotDescribeIt(t *testing.T) {
	t.Parallel()

	const term = "pt-no-such-terminal"
	if hostKnows(t, term) {
		t.Skipf("host unexpectedly has a %s entry", term)
	}

	srv, c := dialTestServer(t)
	g := (&fakeGuest{}).init(nil, nil)
	srv.setTerminfoHandler(g.handle)

	if got := c.negotiateTerm(context.Background(), term); got != defaultTerm {
		t.Fatalf("negotiateTerm = %q, want the %s fallback", got, defaultTerm)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.installs != 0 {
		t.Fatalf("installed %d entries the host could not describe", g.installs)
	}
}

// TestNegotiateTermSkipsTheRoundTripForSafeNames pins the two cases that must
// not cost a probe: no TERM at all, and a TERM that is already the fallback.
// pt shell pays this on every session, so "no work to do" has to mean no work.
func TestNegotiateTermSkipsTheRoundTripForSafeNames(t *testing.T) {
	t.Parallel()

	srv, c := dialTestServer(t)
	var probes int
	var mu sync.Mutex
	srv.setTerminfoHandler(func(string, io.Reader) int {
		mu.Lock()
		probes++
		mu.Unlock()
		return 1
	})

	for _, hostTerm := range []string{"", defaultTerm} {
		if got := c.negotiateTerm(context.Background(), hostTerm); got != defaultTerm {
			t.Fatalf("negotiateTerm(%q) = %q, want %q", hostTerm, got, defaultTerm)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if probes != 0 {
		t.Fatalf("probed the guest %d times for a name that never needed one", probes)
	}
}

// TestNegotiateTermFallsBackWhenTheGuestIsGone: a probe against a dead
// connection must produce a usable TERM, not a failed session. Nothing about
// terminfo is worth refusing to open a shell over.
func TestNegotiateTermFallsBackWhenTheGuestIsGone(t *testing.T) {
	t.Parallel()

	_, c := dialTestServer(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := c.negotiateTerm(context.Background(), "xterm-ghostty"); got != defaultTerm {
		t.Fatalf("negotiateTerm = %q, want the %s fallback", got, defaultTerm)
	}
}
