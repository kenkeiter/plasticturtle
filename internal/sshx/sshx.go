// Package sshx wraps golang.org/x/crypto/ssh for the two things pt needs:
// an interactive PTY session, and local port forwards.
//
// No ssh binary is involved. Tunnels are goroutines owned by the supervisor's
// SSH client, which means they die with the supervisor by construction —
// there is no orphaned listener to garbage-collect.
package sshx

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/sys"
)

// Default credentials follow the Tart image convention. They are deliberately
// not configurable through .plasticturtle: that file is checked into repos,
// and secrets do not belong there. The env vars are an escape hatch for images
// that differ.
const (
	DefaultUser     = "admin"
	DefaultPassword = "admin"

	EnvUser     = "PT_SSH_USER"
	EnvPassword = "PT_SSH_PASSWORD"
)

// Credentials are the guest login used for both sessions and tunnels.
type Credentials struct {
	User     string
	Password string
}

// DefaultCredentials returns admin/admin with EnvUser/EnvPassword overrides
// applied.
func DefaultCredentials() Credentials {
	c := Credentials{User: DefaultUser, Password: DefaultPassword}
	// An empty env var is treated as unset: `PT_SSH_USER= pt shell` is far
	// more likely to be an accident than a request to log in as nobody.
	if v := os.Getenv(EnvUser); v != "" {
		c.User = v
	}
	if v := os.Getenv(EnvPassword); v != "" {
		c.Password = v
	}
	return c
}

// Client is a live SSH connection to a guest.
type Client struct {
	conn *ssh.Client

	mu      sync.Mutex
	tunnels map[*Tunnel]struct{}
	closed  bool

	// once/closeErr memoize the teardown so that repeated Close calls —
	// Client.Close is reachable from both the supervisor's teardown and a
	// deferred close on the error path — agree on what happened, and so the
	// second caller does not return before the first has finished.
	once     sync.Once
	closeErr error
}

// clientConfig builds the shared client configuration. Both password and
// keyboard-interactive are offered because sshd images differ in which one
// they advertise for a password login, and a client that offers only the one
// the server does not list fails with an opaque "no supported methods".
func clientConfig(creds Credentials) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: creds.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(creds.Password),
			ssh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = creds.Password
				}
				return answers, nil
			}),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // see Dial's doc comment
	}
}

// Dial opens one connection to addr ("host:port"), making a single attempt.
//
// Host keys are not verified. These are ephemeral VMs that pt itself just
// cloned, reachable only over the local virtio network, and they have no
// stable identity to pin — a known-hosts entry would be regenerated every boot
// and would train the user to click through mismatches. The threat this drops
// (an attacker already on the host's virtio network impersonating a VM that
// exists for minutes) is not one this tool defends against.
func Dial(ctx context.Context, addr string, creds Credentials) (*Client, error) {
	d := net.Dialer{Timeout: ptcfg.TCPProbeTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}

	// crypto/ssh has no context-aware handshake, so cancellation is expressed
	// the only way the transport understands: by closing the socket underneath
	// it. The handshake is otherwise bounded only by ctx, deliberately — a
	// fixed handshake timeout would either be too short for a guest whose sshd
	// just started or too long to be worth having.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })

	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, clientConfig(creds))
	if err != nil {
		stop()
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ssh handshake %s: %w", addr, ctxErr)
		}
		return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	if !stop() {
		// ctx fired during the handshake; the socket is being closed out from
		// under us, so the connection we just built is not usable.
		_ = sc.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", addr, ctx.Err())
	}

	return &Client{conn: ssh.NewClient(sc, chans, reqs), tunnels: map[*Tunnel]struct{}{}}, nil
}

// DialWithRetry dials with exponential backoff between ptcfg.SSHRetryInitial
// and ptcfg.SSHRetryMax until ctx is done. It is for the boot path only: once
// a connection is established, a drop is reported to the user, not retried.
func DialWithRetry(ctx context.Context, addr string, creds Credentials, clk sys.Clock) (*Client, error) {
	backoff := ptcfg.SSHRetryInitial
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, retryFailure(addr, err, lastErr)
		}
		c, err := Dial(ctx, addr, creds)
		if err == nil {
			return c, nil
		}
		lastErr = err

		select {
		case <-clk.After(backoff):
		case <-ctx.Done():
			return nil, retryFailure(addr, ctx.Err(), lastErr)
		}

		// Doubling is capped rather than unbounded: a guest that takes the
		// full boot timeout should still be polled every couple of seconds so
		// the shell attaches promptly once sshd is up.
		if backoff *= 2; backoff > ptcfg.SSHRetryMax {
			backoff = ptcfg.SSHRetryMax
		}
	}
}

// retryFailure reports why waiting stopped and what the guest last said, since
// "context deadline exceeded" alone tells a user nothing about the VM.
func retryFailure(addr string, cause, last error) error {
	if last == nil {
		return fmt.Errorf("ssh dial %s: %w", addr, cause)
	}
	return fmt.Errorf("ssh dial %s: %w (last attempt: %v)", addr, cause, last)
}

// Close tears down the connection and every tunnel opened on it.
func (c *Client) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		// Snapshot and release: Tunnel.Close calls back into forget(), which
		// takes this same mutex.
		live := make([]*Tunnel, 0, len(c.tunnels))
		for t := range c.tunnels {
			live = append(live, t)
		}
		c.tunnels = nil
		c.mu.Unlock()

		// Tunnels first: closing the transport underneath a live forward would
		// leave its goroutines to discover the failure instead of being told.
		for _, t := range live {
			_ = t.Close()
		}
		c.closeErr = c.conn.Close()
	})
	return c.closeErr
}

// track registers t so Client.Close tears it down. It reports false if the
// client is already closed, in which case the caller must not hand out t.
func (c *Client) track(t *Tunnel) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.tunnels[t] = struct{}{}
	return true
}

// forget drops a tunnel closed on its own, so a long-lived supervisor that
// rebuilds forwards does not accumulate dead entries.
func (c *Client) forget(t *Tunnel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tunnels, t)
}

// ProbeTCP dials addr with a short timeout and closes it, reporting whether
// something is listening. It is how the boot path decides sshd is up before
// attempting a real connection.
func ProbeTCP(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, ptcfg.TCPProbeTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("probe %s: %w", addr, err)
	}
	// Nothing is read: an accepted TCP connection is the whole signal, and
	// reading sshd's banner would only add a way to block.
	return conn.Close()
}

// LoginCommand builds the preamble pt shell runs in place of a bare login: it
// lands the user in the project share when one exists, marks the session so
// nested tooling can detect it, and then execs the guest's own login shell.
//
// This is a command string rather than session environment variables because
// sshd rejects unlisted SendEnv/AcceptEnv variables by default, silently.
func LoginCommand(guestProjectPath string) string {
	// exec, not a subshell: the user's shell must own the PTY so that job
	// control, SIGWINCH and the exit status all belong to it directly. SHELL
	// is defaulted because a guest with no SHELL in the environment would
	// otherwise exec the empty string and drop the session instantly.
	const tail = `export PT_IN_VM_SESSION=1; exec "${SHELL:-/bin/sh}" -l`
	if guestProjectPath == "" {
		return `cd "$HOME"; ` + tail
	}
	// The guest path contains spaces (/Volumes/My Shared Files/project), so it
	// is single-quoted; the || fallback covers non-macOS guests where virtiofs
	// shares are not auto-mounted at that path.
	return "cd " + shellQuote(guestProjectPath) + ` 2>/dev/null || cd "$HOME"; ` + tail
}

// shellQuote renders s as a single POSIX sh word. Single quotes suppress every
// expansion, which is what a path from config deserves; an embedded quote is
// closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
