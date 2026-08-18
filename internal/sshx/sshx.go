// Package sshx wraps golang.org/x/crypto/ssh for the two things pt needs:
// an interactive PTY session, and local port forwards.
//
// No ssh binary is involved. Tunnels are goroutines owned by the supervisor's
// SSH client, which means they die with the supervisor by construction —
// there is no orphaned listener to garbage-collect.
package sshx

import (
	"context"
	"io"
	"net"
	"os"

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
func DefaultCredentials() Credentials { panic("TODO(wave1): sshx.DefaultCredentials") }

// Client is a live SSH connection to a guest.
type Client struct{ panicPlaceholder struct{} }

// Dial opens one connection to addr ("host:port"), making a single attempt.
//
// Host keys are not verified. These are ephemeral VMs that pt itself just
// cloned, reachable only over the local virtio network, and they have no
// stable identity to pin — a known-hosts entry would be regenerated every boot
// and would train the user to click through mismatches. The threat this drops
// (an attacker already on the host's virtio network impersonating a VM that
// exists for minutes) is not one this tool defends against.
func Dial(ctx context.Context, addr string, creds Credentials) (*Client, error) {
	panic("TODO(wave1): sshx.Dial")
}

// DialWithRetry dials with exponential backoff between ptcfg.SSHRetryInitial
// and ptcfg.SSHRetryMax until ctx is done. It is for the boot path only: once
// a connection is established, a drop is reported to the user, not retried.
func DialWithRetry(ctx context.Context, addr string, creds Credentials, clk sys.Clock) (*Client, error) {
	panic("TODO(wave1): sshx.DialWithRetry")
}

// Close tears down the connection and every tunnel opened on it.
func (c *Client) Close() error { panic("TODO(wave1)") }

// Interactive runs command on the guest with a PTY attached to tty, mirroring
// its size and TERM, forwarding SIGWINCH on resize, and putting the local
// terminal in raw mode for the duration.
//
// It returns the remote command's exit status, which pt shell exits with. Raw
// mode is always restored, including on panic: leaving a user's terminal in
// raw mode is the worst failure this tool can produce.
func (c *Client) Interactive(ctx context.Context, command string, tty *os.File) (exitCode int, err error) {
	panic("TODO(wave1): sshx.Interactive")
}

// Tunnel is a live local forward.
type Tunnel struct{ panicPlaceholder struct{} }

// Addr is the local listening address.
func (t *Tunnel) Addr() net.Addr { panic("TODO(wave1)") }

// Close stops accepting and closes in-flight connections.
func (t *Tunnel) Close() error { panic("TODO(wave1)") }

// Forward listens on hostAddr and pipes each accepted connection to remoteAddr
// through the SSH connection.
//
// hostAddr must be loopback: pt is a sandboxing tool, and binding a guest's
// port to 0.0.0.0 would expose it to the LAN. Forward rejects non-loopback
// addresses rather than trusting callers.
//
// remoteAddr should be 127.0.0.1:<vmPort>. The dial happens inside the guest,
// so targeting the guest's external IP would miss services bound to loopback,
// which is most of them.
func (c *Client) Forward(ctx context.Context, hostAddr, remoteAddr string, logf func(string, ...any)) (*Tunnel, error) {
	panic("TODO(wave1): sshx.Forward")
}

// ProbeTCP dials addr with a short timeout and closes it, reporting whether
// something is listening. It is how the boot path decides sshd is up before
// attempting a real connection.
func ProbeTCP(ctx context.Context, addr string) error { panic("TODO(wave1): sshx.ProbeTCP") }

// LoginCommand builds the preamble pt shell runs in place of a bare login: it
// lands the user in the project share when one exists, marks the session so
// nested tooling can detect it, and then execs the guest's own login shell.
//
// This is a command string rather than session environment variables because
// sshd rejects unlisted SendEnv/AcceptEnv variables by default, silently.
func LoginCommand(guestProjectPath string) string { panic("TODO(wave1): sshx.LoginCommand") }

// TestServer is an in-process SSH server used to test sessions and tunnels
// without a VM.
type TestServer struct{ panicPlaceholder struct{} }

// NewTestServer starts a server on loopback that accepts Credentials and
// serves direct-tcpip channels by dialing through to real local addresses.
func NewTestServer(creds Credentials) (*TestServer, error) { panic("TODO(wave1): sshx.NewTestServer") }

// Addr is the server's listening address.
func (s *TestServer) Addr() string { panic("TODO(wave1)") }

// SetExecHandler installs a handler invoked for each session command.
func (s *TestServer) SetExecHandler(fn func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int) {
	panic("TODO(wave1)")
}

// Close stops the server.
func (s *TestServer) Close() error { panic("TODO(wave1)") }
