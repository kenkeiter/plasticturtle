// Package ports resolves host-port conflicts and renders the pt ports table.
//
// Conflict resolution happens in the interactive pt shell, never in the
// supervisor: the supervisor is detached and has no terminal to prompt on. The
// shell binds each port to prove it is free, hands the resolved list to the
// supervisor, and releases the probes just before spawning it.
package ports

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"syscall"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
)

// loopbackHost is the only address this package ever binds. pt is a sandboxing
// tool: a forward reachable from the LAN would defeat the point, so the address
// is a constant rather than anything a caller can influence.
const loopbackHost = "127.0.0.1"

// maxPort is the top of the TCP port space. Port 0 means "let the kernel
// choose" and is accepted by Bind but never by Negotiate, which must record a
// concrete number in instance.json.
const maxPort = 65535

// Resolved is one forward after conflict resolution.
type Resolved struct {
	VMPort   int
	HostPort int

	// OriginalHostPort is nonzero only when the configured port was taken.
	OriginalHostPort int
}

// Remapped reports whether the configured port had to be changed.
func (r Resolved) Remapped() bool { return r.OriginalHostPort != 0 }

// Prompter is how Negotiate talks to the user. When Interactive is false — no
// TTY, as in a script or a CI run — Negotiate silently takes the automatic
// port and reports it on Out rather than blocking forever on a read.
type Prompter struct {
	In          io.Reader
	Out         io.Writer
	Interactive bool
}

// Probes are the listeners Negotiate holds to reserve resolved ports. The
// caller must Close them immediately before spawning the supervisor, which
// then rebinds. The gap is a race, but a two-instruction one; the supervisor
// retries once if it loses.
type Probes []net.Listener

// Close releases every held probe.
//
// Closing twice is not an error: pt shell defers Close for the failure paths
// and also calls it explicitly on the success path, immediately before the
// spawn. A second close must not turn a working boot into a reported failure.
func (p Probes) Close() error {
	var errs []error
	for _, ln := range p {
		if ln == nil {
			continue
		}
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Negotiate binds each configured host port on loopback. For each port already
// in use it proposes a free alternative — hostPort+ptcfg.PortRemapOffset if
// that is free, otherwise a kernel-assigned ephemeral port — and prompts the
// user, accepting the proposal on a bare Enter and re-prompting if the typed
// port is also taken.
//
// The resolved list is recorded in instance.json for pt ports to display. It
// is never written back to .plasticturtle: that would change the file's bytes
// and invalidate its trust hash, turning a port collision into a security
// prompt.
//
// Every returned port is still held open in the returned Probes when Negotiate
// succeeds; on any error nothing is held. ctx is honored between ports and
// between prompts, but not during a blocking read on p.In — cancelling a
// terminal read is not something the io.Reader contract allows.
func Negotiate(ctx context.Context, want []config.ResolvedPort, p Prompter) ([]Resolved, Probes, error) {
	resolved := make([]Resolved, 0, len(want))
	probes := make(Probes, 0, len(want))

	// One reader for the whole negotiation. A fresh bufio.Reader per port would
	// discard whatever the previous one buffered past its newline, silently
	// eating the answer to the next question.
	var in *bufio.Reader
	if p.Interactive && p.In != nil {
		in = bufio.NewReader(p.In)
	}
	out := p.Out
	if out == nil {
		out = io.Discard
	}

	// A partially negotiated set is worthless to the caller and would keep
	// ports reserved by a process that is about to give up, so any failure
	// releases everything.
	fail := func(err error) ([]Resolved, Probes, error) {
		_ = probes.Close()
		return nil, nil, err
	}

	for _, w := range want {
		if err := ctx.Err(); err != nil {
			return fail(fmt.Errorf("ports: negotiating vm port %d: %w", w.VMPort, err))
		}
		if w.HostPort < 1 || w.HostPort > maxPort {
			return fail(fmt.Errorf("ports: vm port %d: host port %d is out of range", w.VMPort, w.HostPort))
		}

		if ln, err := Bind(w.HostPort); err == nil {
			resolved = append(resolved, Resolved{VMPort: w.VMPort, HostPort: w.HostPort})
			probes = append(probes, ln)
			continue
		} else if _, unavailable := bindRefusal(err); !unavailable {
			// Something other than a busy or forbidden port — a bad address
			// family, a resource limit — is a real failure, not a conflict to
			// negotiate around.
			return fail(err)
		} else {
			// Reserve the proposal before showing it. Offering a port that a
			// browser's next ephemeral connection could steal while the user
			// reads the question would make the prompt a lie.
			proposal, perr := reserve(w.HostPort + ptcfg.PortRemapOffset)
			if perr != nil {
				return fail(fmt.Errorf("ports: no free host port for vm port %d: %w", w.VMPort, perr))
			}
			reason, _ := bindRefusal(err)
			chosen, cerr := negotiateOne(ctx, in, out, w, reason, proposal)
			if cerr != nil {
				_ = proposal.Close()
				return fail(cerr)
			}
			r := Resolved{VMPort: w.VMPort, HostPort: portOf(chosen), OriginalHostPort: w.HostPort}
			if r.HostPort == w.HostPort {
				// The port was freed between the failed bind and the user's
				// answer. Nothing was remapped, so nothing should say it was.
				r.OriginalHostPort = 0
			}
			resolved = append(resolved, r)
			probes = append(probes, chosen)
		}
	}
	return resolved, probes, nil
}

// negotiateOne resolves a single conflicted port, consuming proposal on
// acceptance and closing it only if the user picks something else. It returns
// the listener that will become a probe, so the chosen port is never released
// between the decision and the caller taking ownership.
func negotiateOne(ctx context.Context, in *bufio.Reader, out io.Writer, w config.ResolvedPort, reason string, proposal net.Listener) (net.Listener, error) {
	proposed := portOf(proposal)

	if in == nil {
		// No TTY. Reading an answer that is never coming would hang pt shell
		// forever in a script or a CI run, so the automatic port is taken and
		// reported rather than offered.
		fmt.Fprintf(out, "Port %d is %s.\nForwarding VM port %d to host port %d instead.\n", w.HostPort, reason, w.VMPort, proposed)
		return proposal, nil
	}

	fmt.Fprintf(out, "Port %d is %s.\n", w.HostPort, reason)
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("ports: prompting for vm port %d: %w", w.VMPort, err)
		}
		fmt.Fprintf(out, "Forward VM port %d to host port [%d]: ", w.VMPort, proposed)

		line, rerr := in.ReadString('\n')
		// A closed stdin is an answer too: it means nobody is going to type
		// anything, ever. Re-prompting would spin at full speed until the
		// process is killed, so end-of-input takes the proposal.
		eof := errors.Is(rerr, io.EOF)
		if rerr != nil && !eof {
			return nil, fmt.Errorf("ports: read host port answer: %w", rerr)
		}
		accept := func() (net.Listener, error) {
			if eof {
				// The terminal echo of the missing Enter.
				fmt.Fprintln(out)
			}
			return proposal, nil
		}

		text := strings.TrimSpace(line)
		if text == "" {
			return accept()
		}

		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > maxPort {
			fmt.Fprintf(out, "%q is not a port number between 1 and %d.\n", text, maxPort)
			if eof {
				return accept()
			}
			continue
		}
		if n == proposed {
			return proposal, nil
		}

		ln, berr := Bind(n)
		if berr == nil {
			_ = proposal.Close()
			return ln, nil
		}
		alsoReason, unavailable := bindRefusal(berr)
		if !unavailable {
			return nil, berr
		}
		fmt.Fprintf(out, "Port %d is also %s.\n", n, alsoReason)
		if eof {
			return accept()
		}
	}
}

// Bind binds a single loopback host port, for the supervisor's rebind.
//
// The error is wrapped but not flattened: callers distinguish "in use" from a
// real failure with errors.Is against syscall.EADDRINUSE.
func Bind(hostPort int) (net.Listener, error) {
	if hostPort < 0 || hostPort > maxPort {
		return nil, fmt.Errorf("ports: host port %d is out of range", hostPort)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(loopbackHost, strconv.Itoa(hostPort)))
	if err != nil {
		return nil, fmt.Errorf("ports: bind %s:%d: %w", loopbackHost, hostPort, err)
	}
	return ln, nil
}

// FreePort returns a free loopback port, preferring prefer when it is
// available.
//
// The port is released before returning, so it is only free at the instant of
// the answer. Negotiate does not use this for that reason; it keeps the
// listener. FreePort exists for callers that genuinely only want a number.
func FreePort(prefer int) (int, error) {
	ln, err := reserve(prefer)
	if err != nil {
		return 0, err
	}
	port := portOf(ln)
	if err := ln.Close(); err != nil {
		return 0, fmt.Errorf("ports: release probe on %d: %w", port, err)
	}
	return port, nil
}

// reserve returns a held listener on a free loopback port, preferring prefer.
// An out-of-range preference (hostPort+10000 above 65535 is common for ports
// in the ephemeral range) falls through to the kernel's choice rather than
// being an error.
func reserve(prefer int) (net.Listener, error) {
	if prefer >= 1 && prefer <= maxPort {
		if ln, err := Bind(prefer); err == nil {
			return ln, nil
		}
	}
	return Bind(0)
}

// portOf extracts the bound port from a listener created by this package.
// Every such listener is TCP, so the assertion cannot fail; a zero answer would
// still be caught by the range check on whatever it is written into.
func portOf(ln net.Listener) int {
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// bindRefusal classifies a bind failure as a port-availability problem and
// returns the phrase describing it, or reports false for anything else.
//
// EACCES joins EADDRINUSE here because a privileged port below 1024 is just as
// unusable to an unprivileged pt, and offering a remap is a far better answer
// than an error the user can do nothing about.
func bindRefusal(err error) (reason string, unavailable bool) {
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return "in use on the host", true
	case errors.Is(err, syscall.EACCES):
		return "not available to this user", true
	default:
		return "", false
	}
}
