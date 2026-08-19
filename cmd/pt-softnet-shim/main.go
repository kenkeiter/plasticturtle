// Command pt-softnet-shim is Plastic Turtle's domain firewall, packaged as a
// drop-in replacement for the `softnet` binary that Tart spawns.
//
// Tart resolves "softnet" from its PATH and hands the new process the guest's
// virtual network link as stdin (a SOCK_DGRAM socketpair, one datagram per
// ethernet frame). pt puts this shim ahead of the real softnet on that PATH.
// The shim spawns the real softnet as a child on a second socketpair, then
// relays frames between the two — applying an internal/netfw filter that enforces
// the project's domain allowlist by DNS-pinning. With no policy (or an "open"
// one) it relays untouched, so it is safe to sit in the path unconditionally.
//
// Privilege: Tart requires softnet to be reachable as root, so this binary is
// installed setuid-root. Root is used for exactly one thing — spawning the real
// softnet, which needs it for vmnet — after which the long-lived relay drops
// back to the invoking user. The real softnet path is resolved only from trusted
// root-owned locations when privileged, never from the environment.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/netfw"
)

func main() {
	lg := newLogger(os.Getenv("PT_SHIM_LOG"))
	lg.printf("start argv=%v euid=%d ruid=%d", os.Args[1:], os.Geteuid(), os.Getuid())

	if err := run(lg); err != nil {
		lg.printf("fatal: %v", err)
		fmt.Fprintln(os.Stderr, "pt-softnet-shim:", err)
		os.Exit(1)
	}
}

func run(lg *logger) error {
	privileged := isPrivileged()

	realPath, err := resolveRealSoftnet(privileged, os.Getenv, os.Stat)
	if err != nil {
		return err
	}
	lg.printf("real softnet: %s", realPath)

	// The second socketpair: fds[0] is ours (we relay on it), fds[1] becomes the
	// child's stdin — the same fd-as-stdin arrangement Tart uses for us, so the
	// link survives across the child's own privilege handling.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("socketpair: %w", err)
	}
	setSocketBuffers(fds[0])
	setSocketBuffers(fds[1])
	shimSide := os.NewFile(uintptr(fds[0]), "softnet-shim-side")
	childStdin := os.NewFile(uintptr(fds[1]), "softnet-child-stdin")
	tartSide := os.Stdin // the link Tart handed us

	// Spawn the real softnet with our argv forwarded verbatim; it understands the
	// tart-supplied flags, we do not need to. It inherits our (root) privileges
	// and does its own vmnet setup and privdrop.
	child := exec.Command(realPath, os.Args[1:]...)
	child.Stdin = childStdin
	child.Stdout = os.Stdout // control-fd channel when Tart uses one
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		return fmt.Errorf("start real softnet: %w", err)
	}
	childStdin.Close()
	lg.printf("real softnet running pid=%d", child.Process.Pid)

	// Root is no longer needed: drop to the invoking user before the long-lived
	// relay and before touching any user-controlled input (the policy file).
	if err := dropPrivileges(); err != nil {
		_ = child.Process.Kill()
		return fmt.Errorf("dropping privileges: %w", err)
	}
	lg.printf("privileges dropped to uid=%d gid=%d", os.Getuid(), os.Getgid())

	filter, desc := buildFilter(lg)
	lg.printf("filter: %s", desc)

	forwardSignals(child, lg)

	var egress, ingress stats
	var egressDecide, ingressDecide verdictFunc
	if filter != nil {
		egressDecide = filter.EgressFromGuest
		ingressDecide = filter.IngressToGuest
	}

	errc := make(chan error, 2)
	go func() { errc <- relayFiltered(shimSide, tartSide, egressDecide, &egress) }()
	go func() { errc <- relayFiltered(tartSide, shimSide, ingressDecide, &ingress) }()

	stop := make(chan struct{})
	go logStats(lg, &egress, &ingress, stop)

	waitErr := child.Wait()
	close(stop)
	lg.printf("real softnet exited (%v); egress frames=%d passed=%d dropped=%d rewrote=%d; ingress frames=%d passed=%d dropped=%d rewrote=%d",
		waitErr,
		egress.frames.Load(), egress.passed.Load(), egress.dropped.Load(), egress.rewrote.Load(),
		ingress.frames.Load(), ingress.passed.Load(), ingress.dropped.Load(), ingress.rewrote.Load())

	// Drain a relay error only to log it; the child's exit is the real signal.
	select {
	case err := <-errc:
		if err != nil {
			lg.printf("relay ended: %v", err)
		}
	default:
	}

	if ee, ok := waitErr.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
	return waitErr
}

// buildFilter reads the policy file named by PT_NETFW_POLICY and returns a
// Filter, or (nil, reason) for pass-through. A policy that is absent, open, or
// unreadable yields pass-through: this binary never fails a boot over its own
// filtering, it only ever adds restriction when explicitly and correctly asked.
func buildFilter(lg *logger) (*netfw.Filter, string) {
	path := os.Getenv("PT_NETFW_POLICY")
	if path == "" {
		return nil, "pass-through (no PT_NETFW_POLICY)"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Sprintf("pass-through (policy unreadable: %v)", err)
	}
	doc, err := netfw.ParsePolicy(raw)
	if err != nil {
		return nil, fmt.Sprintf("pass-through (policy invalid: %v)", err)
	}
	if !doc.Restricted() {
		return nil, fmt.Sprintf("pass-through (policy %q)", doc.Policy)
	}
	f := netfw.New(netfw.Config{
		Allow:  doc.Allow,
		OnDeny: func(reason string) { lg.printf("DENY %s", reason) },
	})
	return f, fmt.Sprintf("restricted, %d allow patterns", len(doc.Allow))
}

// forwardSignals passes SIGINT/SIGTERM to the child. Tart stops softnet with
// SIGINT; the child's exit is what ends the relay.
func forwardSignals(child *exec.Cmd, lg *logger) {
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sigc {
			lg.printf("forwarding %v to child", s)
			_ = child.Process.Signal(s)
		}
	}()
}

func logStats(lg *logger, egress, ingress *stats, stop <-chan struct{}) {
	if !lg.enabled() {
		return
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			lg.printf("egress frames=%d dropped=%d rewrote=%d | ingress frames=%d dropped=%d rewrote=%d",
				egress.frames.Load(), egress.dropped.Load(), egress.rewrote.Load(),
				ingress.frames.Load(), ingress.dropped.Load(), ingress.rewrote.Load())
		}
	}
}

func setSocketBuffers(fd int) {
	// Mirror Tart's sizing (Softnet.swift): 1 MiB send, 4x receive.
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 1<<20)
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
}
