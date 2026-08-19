package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/sshx"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/tart"
)

// errBootTimeout is what every step of the boot reports once the overall
// deadline has passed, so the log names the timeout rather than whichever
// syscall happened to be cancelled by it.
var errBootTimeout = fmt.Errorf("did not become reachable within %s", ptcfg.BootTimeout)

// boot clones, configures, starts and connects to the guest.
//
// ptcfg.BootTimeout bounds the whole sequence rather than any single step: a
// clone that takes 90 seconds and an sshd that takes 90 more is a failed boot
// even though neither step was individually slow, and that is the only bound a
// user waiting at a spinner can reason about.
func (r *run) boot(ctx, vmCtx context.Context) error {
	if err := r.checkTrust(); err != nil {
		return err
	}
	if err := r.checkMounts(); err != nil {
		return err
	}

	bootCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	expired := make(chan struct{})
	defer close(expired)
	go func() {
		select {
		case <-r.clk.After(ptcfg.BootTimeout):
			cancel()
		case <-r.stopped:
			// A SIGTERM during a two-minute boot must not wait out the boot.
			// Cancelling here is what aborts a dial or a probe already in
			// flight; the loops below notice on their own.
			cancel()
		case <-expired:
		}
	}()

	if err := r.d.Tart.Clone(bootCtx, r.p.Config.Image, r.p.InstanceName); err != nil {
		return fmt.Errorf("clone %s: %w", r.p.Config.Image, err)
	}
	// From here on there is a clone on disk, so every failure path below has to
	// go through teardown rather than simply returning.
	r.cloned = true
	r.logf("cloned %s from %s", r.p.InstanceName, r.p.Config.Image)

	if r.p.Config.CPU != 0 || r.p.Config.Memory != 0 {
		if err := r.d.Tart.Set(bootCtx, r.p.InstanceName, r.p.Config.CPU, r.p.Config.Memory); err != nil {
			return fmt.Errorf("set resources: %w", err)
		}
		r.logf("resources: cpu=%d memory=%d MiB (zero means inherit)", r.p.Config.CPU, r.p.Config.Memory)
	}

	softnet, netEnv, err := r.networkRunOpts()
	if err != nil {
		return err
	}
	proc, err := r.d.Tart.Run(vmCtx, r.p.InstanceName, tart.RunOpts{
		// Item 12 of the implementation plan: honored, not forced by the
		// wrapper. Omitting it opens a UI window for every VM pt boots.
		NoGraphics: true,
		Dirs:       dirShares(r.p.Config.Mounts),
		Softnet:    softnet,
		Env:        netEnv,
	})
	if err != nil {
		return fmt.Errorf("run %s: %w", r.p.InstanceName, err)
	}
	r.booted = true
	r.watchChild(proc)
	r.logf("started vm %s (pid %d)", r.p.InstanceName, proc.Pid())

	ip, err := r.waitForIP(bootCtx)
	if err != nil {
		return err
	}
	r.vmIP = ip
	r.logf("guest address is %s", ip)

	if err := r.checkVMNetCollision(); err != nil {
		return err
	}

	addr := net.JoinHostPort(ip, strconv.Itoa(sshPort))
	if err := r.waitForSSHD(bootCtx, addr); err != nil {
		return err
	}

	cl, err := sshx.DialWithRetry(bootCtx, addr, r.d.Creds, r.clk)
	if err != nil {
		// A dial that ran out of time reports a cancelled context, which says
		// nothing. Lead with why the waiting stopped and keep the guest's last
		// answer, which is the part that names a wrong password or a refused
		// connection.
		if ierr := r.bootInterrupted(bootCtx); ierr != nil {
			return fmt.Errorf("%w (last ssh error: %v)", ierr, err)
		}
		return fmt.Errorf("connect to guest: %w", err)
	}
	r.sshc = cl

	return r.openTunnels(ctx)
}

// watchChild turns the tart run child's exit into a teardown request.
//
// The goroutine is the third watcher: it cannot select on r.stopped because
// Process.Wait does not take a context, so instead it closes childDone, which
// boot and teardown both wait on. A stop that was already requested wins,
// leaving this exit correctly recorded as a consequence rather than a cause.
func (r *run) watchChild(proc sys.Process) {
	go func() {
		err := proc.Wait()
		r.childErr = err
		close(r.childDone)
		if err != nil {
			r.requestStop(stopVMDied, "tart run exited: %v", err)
			return
		}
		r.requestStop(stopVMDied, "tart run exited")
	}()
}

// waitForIP polls tart ip until the guest has a DHCP lease.
func (r *run) waitForIP(ctx context.Context) (string, error) {
	for {
		if err := r.bootInterrupted(ctx); err != nil {
			return "", err
		}
		ip, err := r.d.Tart.IP(ctx, r.p.InstanceName)
		if err == nil && ip != "" {
			return ip, nil
		}
		if err != nil && !errors.Is(err, tart.ErrNoIP) {
			// ErrNoIP is the expected answer for most of a boot. Anything else
			// is worth a line in the log, but not worth abandoning the boot
			// over: a single failed tart invocation is not a failed VM.
			r.logf("waiting for address: %v", err)
		}
		if err := r.sleep(ctx, ptcfg.IPPollInterval); err != nil {
			return "", err
		}
	}
}

// waitForSSHD probes the guest's ssh port until something accepts.
//
// The probe exists so the log distinguishes "the guest is not listening yet"
// from "the guest refused our credentials"; the dial that follows would retry
// on its own, but its failures all look alike.
func (r *run) waitForSSHD(ctx context.Context, addr string) error {
	backoff := ptcfg.SSHRetryInitial
	for {
		if err := r.bootInterrupted(ctx); err != nil {
			return err
		}
		if err := sshx.ProbeTCP(ctx, addr); err == nil {
			r.logf("sshd is accepting connections on %s", addr)
			return nil
		}
		if err := r.sleep(ctx, backoff); err != nil {
			return err
		}
		if backoff *= 2; backoff > ptcfg.SSHRetryMax {
			backoff = ptcfg.SSHRetryMax
		}
	}
}

// bootInterrupted reports the one-line reason the boot cannot continue.
//
// The reasons are ordered by how much they explain: a dead child says why the
// context was cancelled, and a requested stop says why nothing was waited for.
// Reporting the bare cancellation instead would tell a user reading
// supervisor.log only that something gave up.
func (r *run) bootInterrupted(ctx context.Context) error {
	select {
	case <-r.childDone:
		if r.childErr != nil {
			return fmt.Errorf("vm exited during boot: %w", r.childErr)
		}
		return errors.New("vm exited during boot")
	default:
	}
	select {
	case <-r.stopped:
		return fmt.Errorf("boot abandoned: %s", r.stopReason)
	default:
	}
	if ctx.Err() != nil {
		return errBootTimeout
	}
	return nil
}

// sleep waits d, or returns early with the reason the boot is over. It goes
// through the injected clock so that a 120-second boot timeout is testable in
// microseconds.
func (r *run) sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-r.clk.After(d):
		return nil
	case <-r.childDone:
		return r.bootInterrupted(ctx)
	case <-r.stopped:
		return r.bootInterrupted(ctx)
	case <-ctx.Done():
		return r.bootInterrupted(ctx)
	}
}

// openTunnels establishes every forward the shell negotiated.
//
// A forward that cannot be established fails the boot. The alternative —
// running without it — would leave instance.json advertising a port that
// nothing is listening on, so pt ports would report a forward that silently
// is not there.
func (r *run) openTunnels(ctx context.Context) error {
	for _, f := range r.p.Ports {
		hostAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.HostPort))
		remote := guestAddrs(r.vmIP, f.VMPort)

		_, err := r.sshc.ForwardAny(ctx, hostAddr, remote, r.d.Logf)
		if err != nil {
			// pt shell released its probe listener microseconds before spawning
			// us, so a bind failure here is most likely somebody else winning
			// that gap. One retry costs a quarter second and covers it.
			r.logf("forward %s: %v; retrying once", hostAddr, err)
			if serr := r.sleep(ctx, ptcfg.SSHRetryInitial); serr != nil {
				return serr
			}
			if _, err = r.sshc.ForwardAny(ctx, hostAddr, remote, r.d.Logf); err != nil {
				return fmt.Errorf("forward host port %d: %w", f.HostPort, err)
			}
		}
		r.logf("forwarding 127.0.0.1:%d -> %s in guest", f.HostPort, remote)
	}
	return nil
}

// guestAddrs lists the addresses inside the guest a forward may dial, in
// preference order.
//
// Loopback first: most services bind only there, and it is the address the
// guest itself would use. The guest's external address follows as a fallback
// for a service bound only to its LAN interface.
//
// This deliberately does not probe. The previous implementation probed the
// external address from the HOST at tunnel-setup time and picked a winner --
// which asked the wrong machine (the dial happens inside the guest) at the
// wrong moment (nothing is listening on either candidate seconds after boot),
// so it answered "no" every time and hardcoded loopback. The choice belongs to
// the first real connection; see Client.ForwardAny.
func guestAddrs(vmIP string, vmPort int) []string {
	loopback := net.JoinHostPort("127.0.0.1", strconv.Itoa(vmPort))
	ip := net.ParseIP(vmIP)
	if vmIP == "" || ip == nil || ip.IsLoopback() {
		return []string{loopback}
	}
	return []string{loopback, net.JoinHostPort(vmIP, strconv.Itoa(vmPort))}
}

// checkMounts re-verifies every share's host path.
//
// pt shell checked these already and reported them on a terminal, which is the
// error message that matters. This second check is defense in depth: the
// supervisor is what hands the paths to tart, and a directory that disappeared
// between the two would otherwise become a confusing tart failure.
// checkTrust refuses to boot a config the user never approved.
//
// pt shell already made this check, and anyone who can invoke `pt _supervise`
// can invoke `tart` directly — so this grants no new capability and is not a
// security boundary. It is here so that the trust decision is layered rather
// than made in exactly one place: _supervise takes a full config (image,
// mounts, modes) on stdin and acts on it, and "the only caller is well-behaved"
// is the kind of assumption that stops being true quietly.
//
// It checks the snapshotted hash against the trust database rather than
// re-hashing the file on disk. The config is deliberately snapshotted at
// creation (spec §6.2), and the file may legitimately have changed or been
// deleted since — re-reading it would refuse boots the design explicitly
// allows.
//
// One benign race remains: re-allowing an edited config in the moment between
// pt shell's check and this one leaves the old hash no longer in the database,
// and this boot is refused. The next pt shell succeeds.
func (r *run) checkTrust() error {
	if r.d.Trust == nil {
		return errors.New("supervisor: no trust database")
	}
	path := r.p.Config.ProjectPath
	allowed, err := r.d.Trust.Check(path, r.p.ConfigHash)
	if err != nil {
		return fmt.Errorf("supervisor: check trust for %s: %w", path, err)
	}
	if !allowed {
		return fmt.Errorf("supervisor: refusing to boot %s: its config is not allowed (%s); run: pt allow", path, r.p.ConfigHash)
	}
	return nil
}

func (r *run) checkMounts() error {
	var errs []error
	for _, m := range r.p.Config.Mounts {
		st, err := os.Stat(m.HostPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
			errs = append(errs, fmt.Errorf("mount %q: host path %s does not exist", m.Name, m.HostPath))
		case err != nil:
			errs = append(errs, fmt.Errorf("mount %q: host path %s: %w", m.Name, m.HostPath, err))
		case !st.IsDir():
			errs = append(errs, fmt.Errorf("mount %q: host path %s is not a directory", m.Name, m.HostPath))
		}
	}
	return errors.Join(errs...)
}

// dirShares renders the resolved mounts as tart --dir arguments.
func dirShares(mounts []config.ResolvedMount) []tart.DirShare {
	out := make([]tart.DirShare, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, tart.DirShare{
			Name:     m.Name,
			HostPath: m.HostPath,
			ReadOnly: m.Mode == config.ModeRO,
		})
	}
	return out
}
