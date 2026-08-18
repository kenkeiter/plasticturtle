package sshx

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
)

// Tunnel is a live local forward.
type Tunnel struct {
	client     *Client
	ln         net.Listener
	remoteAddr string

	// candidates are the guest-side addresses to try, in order. chosen latches
	// whichever last answered, so the cost of trying a dead one is paid once
	// per tunnel rather than once per connection.
	candidates []string
	chosen     string
	logf       func(string, ...any)

	once     sync.Once
	closeErr error
	wg       sync.WaitGroup

	mu     sync.Mutex
	closed bool
	// stopCtx detaches the tunnel from the context it was created with, so a
	// tunnel closed by hand does not leave a callback attached to a
	// long-lived supervisor context. It is guarded because an already-expired
	// context runs its callback before Forward has finished storing it.
	stopCtx func() bool
	// conns is every connection currently being piped. Close needs it because
	// closing the listener stops new connections but says nothing about a
	// session that is mid-transfer, and the supervisor's teardown must not
	// wait on an idle-forever TCP connection.
	conns map[io.Closer]struct{}
}

// Addr is the local listening address.
func (t *Tunnel) Addr() net.Addr { return t.ln.Addr() }

// Close stops accepting and closes in-flight connections.
func (t *Tunnel) Close() error {
	t.once.Do(func() {
		t.closeErr = t.ln.Close()

		t.mu.Lock()
		t.closed = true
		conns := t.conns
		t.conns = nil
		stop := t.stopCtx
		t.mu.Unlock()

		if stop != nil {
			stop()
		}
		// Outside the lock: each Close wakes a piping goroutine that will take
		// the same lock on its way out.
		for c := range conns {
			_ = c.Close()
		}
		if t.client != nil {
			t.client.forget(t)
		}
	})
	// Every caller, not just the first, waits for the goroutines to finish, so
	// that a returned Close means the tunnel is genuinely quiet.
	t.wg.Wait()
	return t.closeErr
}

// add registers c for teardown, reporting false if the tunnel is already
// closing — in which case the caller owns closing c immediately.
func (t *Tunnel) add(c io.Closer) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.conns[c] = struct{}{}
	return true
}

func (t *Tunnel) remove(c io.Closer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conns != nil {
		delete(t.conns, c)
	}
}

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
	return c.ForwardAny(ctx, hostAddr, []string{remoteAddr}, logf)
}

// ForwardAny is Forward with more than one candidate guest-side address.
//
// The tunnel tries them in order on its first connection and latches whichever
// answers, so a caller that cannot know in advance which interface a guest
// service will bind to does not have to guess. The cost of a dead candidate is
// paid once per tunnel, not once per connection.
//
// This exists because the question "which address will the guest answer on?"
// has no true answer at tunnel-setup time — nothing is listening on any of them
// yet — so it must be asked lazily, by a real connection.
func (c *Client) ForwardAny(ctx context.Context, hostAddr string, remoteAddrs []string, logf func(string, ...any)) (*Tunnel, error) {
	if len(remoteAddrs) == 0 {
		return nil, fmt.Errorf("forward %s: no guest address given", hostAddr)
	}
	if err := requireLoopback(hostAddr); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		// Cheaper than handing back a tunnel that the context is about to tear
		// down anyway, and it keeps a cancelled supervisor from reporting
		// forwards it never really established.
		return nil, fmt.Errorf("forward listen %s: %w", hostAddr, err)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", hostAddr)
	if err != nil {
		return nil, fmt.Errorf("forward listen %s: %w", hostAddr, err)
	}

	t := &Tunnel{
		client:     c,
		ln:         ln,
		remoteAddr: remoteAddrs[0],
		candidates: remoteAddrs,
		logf:       logf,
		conns:      map[io.Closer]struct{}{},
	}
	// The accept goroutine is counted before the tunnel becomes reachable by
	// anyone who could Close it: a Wait that observes a zero counter and then
	// races an Add is a WaitGroup misuse panic, not merely a lost goroutine.
	t.wg.Add(1)
	go t.accept()

	// The tunnel's lifetime is the caller's context as well as its own Close:
	// the supervisor cancels one context on teardown and expects every forward
	// to go with it.
	stop := context.AfterFunc(ctx, func() { _ = t.Close() })
	t.mu.Lock()
	alreadyClosed := t.closed
	if !alreadyClosed {
		t.stopCtx = stop
	}
	t.mu.Unlock()
	if alreadyClosed {
		// ctx was already done and the callback beat us here.
		stop()
	}

	if !c.track(t) {
		_ = t.Close()
		return nil, fmt.Errorf("forward listen %s: client is closed", hostAddr)
	}
	return t, nil
}

// accept serves the listener until Close (or the context) shuts it down.
func (t *Tunnel) accept() {
	defer t.wg.Done()
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			t.mu.Lock()
			closed := t.closed
			t.mu.Unlock()
			if !closed {
				t.logf("tunnel %s: accept failed: %v", t.ln.Addr(), err)
			}
			return
		}
		t.wg.Add(1)
		go t.handle(conn)
	}
}

// handle pipes one accepted connection to the guest. One goroutine per
// connection is the whole design: a forward with a stuck consumer must not
// stall the others.
func (t *Tunnel) handle(local net.Conn) {
	defer t.wg.Done()

	if !t.add(local) {
		_ = local.Close()
		return
	}
	defer func() {
		t.remove(local)
		_ = local.Close()
	}()

	remote, err := t.dialGuest()
	if err != nil {
		// Logged rather than fatal: the guest service may simply not be up
		// yet, and killing the listener would turn a transient into a restart.
		t.logf("tunnel %s -> %s: dial in guest failed: %v", t.ln.Addr(), t.remoteAddr, err)
		return
	}
	if !t.add(remote) {
		_ = remote.Close()
		return
	}
	defer func() {
		t.remove(remote)
		_ = remote.Close()
	}()

	pipe(local, remote)
}

// pipe copies in both directions until both are done. Neither half is allowed
// to end the other: a client that half-closes after sending its request — the
// shape of most one-shot protocols — is still waiting for the answer, and
// tearing the connection down on the first EOF would truncate it. Tunnel.Close
// is what unblocks a pair that would otherwise sit here forever.
func pipe(a, b io.ReadWriter) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyHalf(a, b) }()
	go func() { defer wg.Done(); copyHalf(b, a) }()
	wg.Wait()
}

// copyHalf drains src into dst and then shuts down only dst's write half, so
// the peer sees the EOF it is waiting for. Both a TCP connection and an SSH
// channel support this; anything else gets a full close as a fallback.
func copyHalf(dst io.Writer, src io.Reader) {
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	if c, ok := dst.(io.Closer); ok {
		_ = c.Close()
	}
}

// requireLoopback rejects any bind address that is reachable from off-host.
// The check is on the address rather than the caller because a bug two
// packages away must not be able to publish a sandboxed VM to the network.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("forward host address %q: %w", addr, err)
	}
	if host == "" {
		// ":8080" binds every interface.
		return fmt.Errorf("forward host address %q: must bind loopback explicitly, not all interfaces", addr)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Names other than localhost are refused instead of resolved: a
		// hostname that resolves to a routable address today may not tomorrow.
		return fmt.Errorf("forward host address %q: must be a loopback IP or localhost", addr)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("forward host address %q: refusing to bind non-loopback address", addr)
	}
	return nil
}

// dialGuest opens a connection to the guest, trying the latched address first
// and falling back through the remaining candidates.
//
// The choice is made here, lazily, rather than once at tunnel setup, because at
// setup time nothing is listening on any candidate yet — a probe then can only
// answer "no" and would hardcode the first candidate forever. The first real
// connection is the earliest moment the question has a true answer.
func (t *Tunnel) dialGuest() (net.Conn, error) {
	t.mu.Lock()
	order := make([]string, 0, len(t.candidates))
	if t.chosen != "" {
		order = append(order, t.chosen)
	}
	for _, addr := range t.candidates {
		if addr != t.chosen {
			order = append(order, addr)
		}
	}
	t.mu.Unlock()

	var lastErr error
	for _, addr := range order {
		conn, err := t.client.conn.Dial("tcp", addr)
		if err != nil {
			lastErr = err
			continue
		}
		t.mu.Lock()
		if t.chosen != addr {
			t.chosen = addr
			t.logf("tunnel %s -> guest %s: using this address", t.ln.Addr(), addr)
		}
		t.mu.Unlock()
		return conn, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no guest address to dial")
	}
	return nil, lastErr
}
