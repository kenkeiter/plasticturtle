package ports

import (
	"context"

	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
)

// The port tests bind real loopback sockets rather than mocking the network.
// The whole point of this package is what the kernel says about a port, so a
// fake would be testing the fake.

// occupy binds hostPort and keeps it bound for the rest of the test.
func occupy(t *testing.T, hostPort int) {
	t.Helper()
	ln, err := Bind(hostPort)
	if err != nil {
		t.Fatalf("occupy %d: %v", hostPort, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

// freePorts returns n ports that were free a moment ago. They are held
// simultaneously so the numbers cannot collide with each other.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	held := make([]net.Listener, 0, n)
	out := make([]int, 0, n)
	for range n {
		ln, err := Bind(0)
		if err != nil {
			t.Fatalf("reserve ephemeral port: %v", err)
		}
		held = append(held, ln)
		out = append(out, portOf(ln))
	}
	for _, ln := range held {
		if err := ln.Close(); err != nil {
			t.Fatalf("release ephemeral port: %v", err)
		}
	}
	return out
}

// takenBase occupies a port whose +PortRemapOffset neighbour is free, which is
// the case the prompt's default answer is built for. Ephemeral ports are no
// good here: on darwin they start at 49152, so base+10000 is out of range.
func takenBase(t *testing.T) int {
	t.Helper()
	for base := 20000; base < 30000; base++ {
		ln, err := Bind(base)
		if err != nil {
			continue
		}
		probe, perr := Bind(base + ptcfg.PortRemapOffset)
		if perr != nil {
			_ = ln.Close()
			continue
		}
		if err := probe.Close(); err != nil {
			t.Fatalf("release remap probe: %v", err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		return base
	}
	t.Fatal("no port in 20000..30000 with a free +10000 neighbour")
	return 0
}

// blockingReader fails the test if anything reads it. Negotiate must never
// touch stdin when there is no TTY to answer with.
type blockingReader struct{ t *testing.T }

func (b blockingReader) Read([]byte) (int, error) {
	b.t.Fatal("Negotiate read from In in non-interactive mode")
	return 0, io.EOF
}

func negotiate(t *testing.T, want []config.ResolvedPort, input string, interactive bool) ([]Resolved, Probes, string) {
	t.Helper()
	var out strings.Builder
	p := Prompter{Out: &out, Interactive: interactive}
	if interactive {
		p.In = strings.NewReader(input)
	} else {
		p.In = blockingReader{t}
	}

	// Every case here must terminate on its own. A prompt loop that spins on
	// EOF would otherwise hang the whole package's test binary.
	type result struct {
		resolved []Resolved
		probes   Probes
		err      error
	}
	done := make(chan result, 1)
	go func() {
		r, pr, err := Negotiate(context.Background(), want, p)
		done <- result{r, pr, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Negotiate: %v", res.err)
		}
		t.Cleanup(func() { _ = res.probes.Close() })
		return res.resolved, res.probes, out.String()
	case <-time.After(10 * time.Second):
		t.Fatal("Negotiate did not return within 10s")
		return nil, nil, ""
	}
}

func TestNegotiateAllPortsFree(t *testing.T) {
	free := freePorts(t, 2)
	want := []config.ResolvedPort{
		{VMPort: 3000, HostPort: free[0]},
		{VMPort: 5432, HostPort: free[1]},
	}
	got, probes, out := negotiate(t, want, "", true)

	if len(got) != 2 || len(probes) != 2 {
		t.Fatalf("got %d resolved / %d probes, want 2/2", len(got), len(probes))
	}
	for i, r := range got {
		if r.VMPort != want[i].VMPort || r.HostPort != want[i].HostPort {
			t.Errorf("resolved[%d] = %+v, want vm %d host %d", i, r, want[i].VMPort, want[i].HostPort)
		}
		if r.Remapped() {
			t.Errorf("resolved[%d] reports a remap for a free port", i)
		}
		if portOf(probes[i]) != want[i].HostPort {
			t.Errorf("probe[%d] holds %d, want %d", i, portOf(probes[i]), want[i].HostPort)
		}
	}
	if out != "" {
		t.Errorf("Negotiate wrote %q with nothing to negotiate", out)
	}
}

func TestNegotiateBareEnterAcceptsProposal(t *testing.T) {
	base := takenBase(t)
	got, _, out := negotiate(t, []config.ResolvedPort{{VMPort: base, HostPort: base}}, "\n", true)

	if len(got) != 1 {
		t.Fatalf("got %d resolved, want 1", len(got))
	}
	if got[0].HostPort != base+ptcfg.PortRemapOffset {
		t.Errorf("host port = %d, want %d", got[0].HostPort, base+ptcfg.PortRemapOffset)
	}
	if got[0].OriginalHostPort != base {
		t.Errorf("original host port = %d, want %d", got[0].OriginalHostPort, base)
	}
	// The prompt is a documented user-facing string (spec §8.1).
	wantLine := fmt.Sprintf("Port %d is in use on the host.\n", base)
	wantPrompt := fmt.Sprintf("Forward VM port %d to host port [%d]: ", base, base+ptcfg.PortRemapOffset)
	if !strings.Contains(out, wantLine) || !strings.Contains(out, wantPrompt) {
		t.Errorf("prompt output:\n%s\nwant it to contain %q and %q", out, wantLine, wantPrompt)
	}
}

func TestNegotiateRepromptsWhenTypedPortIsAlsoTaken(t *testing.T) {
	base := takenBase(t)
	alsoTaken := freePorts(t, 1)[0]
	occupy(t, alsoTaken)

	input := fmt.Sprintf("%d\n\n", alsoTaken)
	got, _, out := negotiate(t, []config.ResolvedPort{{VMPort: base, HostPort: base}}, input, true)

	if got[0].HostPort != base+ptcfg.PortRemapOffset {
		t.Errorf("host port = %d, want the proposal %d", got[0].HostPort, base+ptcfg.PortRemapOffset)
	}
	if want := fmt.Sprintf("Port %d is also in use on the host.\n", alsoTaken); !strings.Contains(out, want) {
		t.Errorf("output:\n%s\nwant it to contain %q", out, want)
	}
	if n := strings.Count(out, "Forward VM port"); n != 2 {
		t.Errorf("prompted %d times, want 2 (initial + re-prompt)", n)
	}
}

func TestNegotiateRepromptsOnGarbage(t *testing.T) {
	base := takenBase(t)
	got, _, out := negotiate(t, []config.ResolvedPort{{VMPort: base, HostPort: base}}, "not-a-port\n\n", true)

	if got[0].HostPort != base+ptcfg.PortRemapOffset {
		t.Errorf("host port = %d, want the proposal %d", got[0].HostPort, base+ptcfg.PortRemapOffset)
	}
	if !strings.Contains(out, `"not-a-port" is not a port number`) {
		t.Errorf("output:\n%s\nwant a complaint about the typed value", out)
	}
	if n := strings.Count(out, "Forward VM port"); n != 2 {
		t.Errorf("prompted %d times, want 2 (initial + re-prompt)", n)
	}
}

func TestNegotiateAcceptsTypedFreePort(t *testing.T) {
	base := takenBase(t)
	pick := freePorts(t, 1)[0]

	got, probes, _ := negotiate(t, []config.ResolvedPort{{VMPort: base, HostPort: base}}, fmt.Sprintf("%d\n", pick), true)
	if got[0].HostPort != pick {
		t.Fatalf("host port = %d, want the typed %d", got[0].HostPort, pick)
	}
	if got[0].OriginalHostPort != base {
		t.Errorf("original host port = %d, want %d", got[0].OriginalHostPort, base)
	}
	if portOf(probes[0]) != pick {
		t.Errorf("probe holds %d, want %d", portOf(probes[0]), pick)
	}
	// The rejected proposal must not stay reserved.
	ln, err := Bind(base + ptcfg.PortRemapOffset)
	if err != nil {
		t.Fatalf("proposal port %d still held after the user chose otherwise: %v", base+ptcfg.PortRemapOffset, err)
	}
	_ = ln.Close()
}

func TestNegotiateFallsBackToEphemeralWhenOffsetTaken(t *testing.T) {
	base := takenBase(t)
	occupy(t, base+ptcfg.PortRemapOffset)

	got, _, out := negotiate(t, []config.ResolvedPort{{VMPort: base, HostPort: base}}, "\n", true)
	got0 := got[0]
	switch {
	case got0.HostPort == base || got0.HostPort == base+ptcfg.PortRemapOffset:
		t.Fatalf("host port = %d, want a kernel-assigned port distinct from %d and %d",
			got0.HostPort, base, base+ptcfg.PortRemapOffset)
	case got0.HostPort <= 0:
		t.Fatalf("host port = %d, want a real port", got0.HostPort)
	}
	if got0.OriginalHostPort != base {
		t.Errorf("original host port = %d, want %d", got0.OriginalHostPort, base)
	}
	if want := fmt.Sprintf("[%d]", got0.HostPort); !strings.Contains(out, want) {
		t.Errorf("output:\n%s\nwant the prompt default to be the ephemeral port %s", out, want)
	}
}

func TestNegotiateNonInteractiveTakesProposalAndPrints(t *testing.T) {
	base := takenBase(t)
	// blockingReader fails the test if Negotiate reads; that is the assertion
	// that a script never blocks on an answer nobody will give.
	got, _, out := negotiate(t, []config.ResolvedPort{{VMPort: base, HostPort: base}}, "", false)

	if got[0].HostPort != base+ptcfg.PortRemapOffset {
		t.Errorf("host port = %d, want %d", got[0].HostPort, base+ptcfg.PortRemapOffset)
	}
	if strings.Contains(out, "Forward VM port") && strings.Contains(out, "[") {
		t.Errorf("output:\n%s\nwant a report, not a prompt", out)
	}
	want := fmt.Sprintf("Forwarding VM port %d to host port %d instead.", base, base+ptcfg.PortRemapOffset)
	if !strings.Contains(out, want) {
		t.Errorf("output:\n%s\nwant it to contain %q", out, want)
	}
}

func TestNegotiateEOFTakesProposal(t *testing.T) {
	base := takenBase(t)
	// Empty stdin: the read returns EOF immediately and must not spin.
	got, _, _ := negotiate(t, []config.ResolvedPort{{VMPort: base, HostPort: base}}, "", true)
	if got[0].HostPort != base+ptcfg.PortRemapOffset {
		t.Errorf("host port = %d, want the proposal %d", got[0].HostPort, base+ptcfg.PortRemapOffset)
	}
}

func TestNegotiateEOFAfterGarbageTakesProposal(t *testing.T) {
	base := takenBase(t)
	// Garbage with no trailing newline: the re-prompt path hits EOF, where a
	// naive `continue` would loop forever.
	got, _, out := negotiate(t, []config.ResolvedPort{{VMPort: base, HostPort: base}}, "wat", true)
	if got[0].HostPort != base+ptcfg.PortRemapOffset {
		t.Errorf("host port = %d, want the proposal %d", got[0].HostPort, base+ptcfg.PortRemapOffset)
	}
	if n := strings.Count(out, "Forward VM port"); n != 1 {
		t.Errorf("prompted %d times after EOF, want exactly 1", n)
	}
}

func TestNegotiateRejectsOutOfRangeHostPort(t *testing.T) {
	_, _, err := Negotiate(context.Background(), []config.ResolvedPort{{VMPort: 80, HostPort: 0}}, Prompter{})
	if err == nil {
		t.Fatal("Negotiate accepted host port 0")
	}
}

func TestNegotiateReleasesEverythingOnFailure(t *testing.T) {
	free := freePorts(t, 1)[0]
	// The second entry is invalid, so the first port's probe must not survive.
	_, _, err := Negotiate(context.Background(), []config.ResolvedPort{
		{VMPort: 3000, HostPort: free},
		{VMPort: 80, HostPort: 70000},
	}, Prompter{})
	if err == nil {
		t.Fatal("Negotiate accepted an out-of-range host port")
	}
	ln, berr := Bind(free)
	if berr != nil {
		t.Fatalf("port %d still held after a failed negotiation: %v", free, berr)
	}
	_ = ln.Close()
}

func TestProbesCloseReleasesEveryPort(t *testing.T) {
	free := freePorts(t, 3)
	want := make([]config.ResolvedPort, 0, len(free))
	for i, p := range free {
		want = append(want, config.ResolvedPort{VMPort: 3000 + i, HostPort: p})
	}
	_, probes, _ := negotiate(t, want, "", true)

	for _, p := range free {
		if ln, err := Bind(p); err == nil {
			_ = ln.Close()
			t.Fatalf("port %d was not held by its probe", p)
		} else if _, unavailable := bindRefusal(err); !unavailable {
			t.Fatalf("rebinding held port %d failed for the wrong reason: %v", p, err)
		}
	}
	if err := probes.Close(); err != nil {
		t.Fatalf("Probes.Close: %v", err)
	}
	// The whole contract: the supervisor must be able to rebind immediately.
	for _, p := range free {
		ln, err := Bind(p)
		if err != nil {
			t.Fatalf("rebind %d after Probes.Close: %v", p, err)
		}
		_ = ln.Close()
	}
	// Closing twice is a no-op, because pt shell both defers and calls it.
	if err := probes.Close(); err != nil {
		t.Fatalf("second Probes.Close: %v", err)
	}
}

func TestBindRejectsNonLoopback(t *testing.T) {
	ln, err := Bind(0)
	if err != nil {
		t.Fatalf("Bind(0): %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		t.Fatalf("Bind listened on %v, want loopback", ln.Addr())
	}
}

func TestFreePortPrefersThePreference(t *testing.T) {
	free := freePorts(t, 1)[0]
	got, err := FreePort(free)
	if err != nil {
		t.Fatalf("FreePort: %v", err)
	}
	if got != free {
		t.Errorf("FreePort(%d) = %d, want the preference", free, got)
	}

	occupy(t, free)
	got, err = FreePort(free)
	if err != nil {
		t.Fatalf("FreePort on a taken preference: %v", err)
	}
	if got == free || got <= 0 {
		t.Errorf("FreePort(%d) = %d, want a different free port", free, got)
	}

	// An out-of-range preference is a normal case: any host port above 55535
	// has no +10000 neighbour.
	if got, err = FreePort(70000); err != nil || got <= 0 || got > maxPort {
		t.Errorf("FreePort(70000) = %d, %v; want a valid ephemeral port", got, err)
	}
}
