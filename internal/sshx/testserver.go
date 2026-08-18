package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// TestServer is an in-process SSH server used to test sessions and tunnels
// without a VM.
type TestServer struct {
	ln  net.Listener
	cfg *ssh.ServerConfig

	mu    sync.Mutex
	exec  func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int
	conns map[net.Conn]struct{}
	// Observers for the channel requests the exported handler signature
	// cannot express. Tests use them to assert that a session negotiated the
	// PTY it claims to, and to make pty-req fail on demand.
	onPTY   func(ptyPayload) bool
	onWinch func(windowChangePayload)
	// onTerminfo answers the terminfo negotiation. See setTerminfoHandler.
	onTerminfo func(cmd string, stdin io.Reader) int

	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}
	wg        sync.WaitGroup
}

// NewTestServer starts a server on loopback that accepts Credentials and
// serves direct-tcpip channels by dialing through to real local addresses.
func NewTestServer(creds Credentials) (*TestServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("test server listen: %w", err)
	}
	s, err := newTestServerOn(ln, creds)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	return s, nil
}

// newTestServerOn serves on an existing listener. Tests that need a server to
// appear at an address that was previously refusing connections — the retry
// path — cannot get that from NewTestServer, which chooses its own port.
func newTestServerOn(ln net.Listener, creds Credentials) (*TestServer, error) {
	// Ed25519 rather than RSA: key generation is instantaneous, which matters
	// when every test that touches SSH starts a server.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("test server host key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("test server signer: %w", err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			userOK := subtle.ConstantTimeCompare([]byte(conn.User()), []byte(creds.User)) == 1
			passOK := subtle.ConstantTimeCompare(password, []byte(creds.Password)) == 1
			if userOK && passOK {
				return nil, nil
			}
			return nil, fmt.Errorf("test server: bad credentials")
		},
	}
	cfg.AddHostKey(signer)

	s := &TestServer{
		ln:     ln,
		cfg:    cfg,
		conns:  map[net.Conn]struct{}{},
		closed: make(chan struct{}),
		exec:   func(string, io.Reader, io.Writer, io.Writer) int { return 0 },
	}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Addr is the server's listening address.
func (s *TestServer) Addr() string { return s.ln.Addr().String() }

// SetExecHandler installs a handler invoked for each session command.
func (s *TestServer) SetExecHandler(fn func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exec = fn
}

func (s *TestServer) handler() func(string, io.Reader, io.Writer, io.Writer) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exec
}

// setSessionHooks installs observers for pty-req and window-change. onPTY
// reports whether to accept the request, which is how a test drives the
// failure path that must still restore the local terminal.
func (s *TestServer) setSessionHooks(onPTY func(ptyPayload) bool, onWinch func(windowChangePayload)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onPTY, s.onWinch = onPTY, onWinch
}

func (s *TestServer) hooks() (func(ptyPayload) bool, func(windowChangePayload)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onPTY, s.onWinch
}

// setTerminfoHandler installs the guest's answer to the terminfo probe and
// install commands Interactive runs before every PTY session.
//
// Those commands are answered here rather than by the installed exec handler
// because they are infrastructure, not something the test asked to run: routing
// them through a handler a test wrote for its own command would deadlock the
// moment that handler blocked, and would make every test that merely wants a
// PTY reason about terminfo. The default reports a guest that already knows
// every terminal name, which is the case where negotiation is invisible.
func (s *TestServer) setTerminfoHandler(fn func(cmd string, stdin io.Reader) int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onTerminfo = fn
}

func (s *TestServer) terminfoHandler() func(string, io.Reader) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onTerminfo == nil {
		return func(string, io.Reader) int { return 0 }
	}
	return s.onTerminfo
}

// isTerminfoCommand reports whether cmd is part of the terminfo negotiation
// rather than a command a caller asked to run.
func isTerminfoCommand(cmd string) bool {
	return cmd == terminfoInstallCommand || strings.HasPrefix(cmd, "infocmp ")
}

// Close stops the server.
func (s *TestServer) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.closeErr = s.ln.Close()

		s.mu.Lock()
		conns := s.conns
		s.conns = nil
		s.mu.Unlock()
		for c := range conns {
			_ = c.Close()
		}
	})
	s.wg.Wait()
	return s.closeErr
}

func (s *TestServer) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

func (s *TestServer) serve() {
	defer s.wg.Done()
	for {
		nConn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.conns == nil {
			s.mu.Unlock()
			_ = nConn.Close()
			return
		}
		s.conns[nConn] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go s.handshake(nConn)
	}
}

func (s *TestServer) handshake(nConn net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		if s.conns != nil {
			delete(s.conns, nConn)
		}
		s.mu.Unlock()
		_ = nConn.Close()
	}()

	sConn, chans, reqs, err := ssh.NewServerConn(nConn, s.cfg)
	if err != nil {
		return
	}
	defer sConn.Close()

	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		switch newCh.ChannelType() {
		case "session":
			s.wg.Add(1)
			go s.session(newCh)
		case "direct-tcpip":
			s.wg.Add(1)
			go s.directTCPIP(newCh)
		default:
			_ = newCh.Reject(ssh.UnknownChannelType, newCh.ChannelType())
		}
	}
}

// The wire payloads of the requests this server understands, in the field
// order RFC 4254 defines for them.
type execPayload struct{ Command string }

type ptyPayload struct {
	Term          string
	Columns, Rows uint32
	Width, Height uint32
	Modes         string
}

type windowChangePayload struct {
	Columns, Rows uint32
	Width, Height uint32
}

type directTCPIPPayload struct {
	DestAddr string
	DestPort uint32
	OrigAddr string
	OrigPort uint32
}

// session serves one session channel: pty-req and env are acknowledged, and
// the first exec or shell request runs the installed handler and reports its
// exit status the way sshd does.
func (s *TestServer) session(newCh ssh.NewChannel) {
	defer s.wg.Done()

	ch, reqs, err := newCh.Accept()
	if err != nil {
		return
	}

	started := false
	onPTY, onWinch := s.hooks()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var p ptyPayload
			ok := ssh.Unmarshal(req.Payload, &p) == nil
			if ok && onPTY != nil {
				ok = onPTY(p)
			}
			if req.WantReply {
				_ = req.Reply(ok, nil)
			}
		case "window-change":
			var p windowChangePayload
			if err := ssh.Unmarshal(req.Payload, &p); err == nil && onWinch != nil {
				onWinch(p)
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "env", "signal":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "exec", "shell":
			if started {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}
			cmd := ""
			if req.Type == "exec" {
				var p execPayload
				if err := ssh.Unmarshal(req.Payload, &p); err != nil {
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
					continue
				}
				cmd = p.Command
			}
			started = true
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			s.wg.Add(1)
			go s.run(ch, cmd)
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// run executes the handler and closes the channel with an exit-status, which
// is the mechanism Interactive's exit-code propagation is built on.
func (s *TestServer) run(ch ssh.Channel, cmd string) {
	defer s.wg.Done()

	var code int
	if isTerminfoCommand(cmd) {
		code = s.terminfoHandler()(cmd, ch)
	} else {
		code = s.handler()(cmd, ch, ch, ch.Stderr())
	}

	_ = ch.CloseWrite()
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
	_ = ch.Close()
}

// directTCPIP dials the requested address on the host and pipes it to the
// channel. Because the "guest" is this process, a forward under test reaches
// a real local listener, which is what makes an end-to-end tunnel test
// possible without a VM.
func (s *TestServer) directTCPIP(newCh ssh.NewChannel) {
	defer s.wg.Done()

	var p directTCPIPPayload
	if err := ssh.Unmarshal(newCh.ExtraData(), &p); err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
		return
	}
	if s.isClosed() {
		_ = newCh.Reject(ssh.ConnectionFailed, "server closed")
		return
	}

	target := net.JoinHostPort(p.DestAddr, fmt.Sprint(p.DestPort))
	conn, err := net.Dial("tcp", target)
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	defer conn.Close()

	ch, reqs, err := newCh.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	go ssh.DiscardRequests(reqs)

	// Half-closes are forwarded rather than collapsed into a full close, the
	// way a real sshd's forwarding does: a client that shuts down its write
	// half is still owed the response.
	done := make(chan struct{})
	go func() { defer close(done); pipe(ch, conn) }()
	select {
	case <-done:
	case <-s.closed:
	}
}
