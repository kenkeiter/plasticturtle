package sshx

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// testCreds are what every in-process server in this package accepts.
var testCreds = Credentials{User: "admin", Password: "admin"}

// dialTestServer starts a TestServer and a client connected to it, both
// cleaned up when the test ends.
func dialTestServer(t *testing.T) (*TestServer, *Client) {
	t.Helper()

	srv, err := NewTestServer(testCreds)
	if err != nil {
		t.Fatalf("NewTestServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, srv.Addr(), testCreds)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return srv, c
}

// startEcho runs a listener that echoes everything back, standing in for a
// service inside the guest.
func startEcho(t *testing.T) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln
}

func TestForwardRoundTripsAndClosesInFlight(t *testing.T) {
	t.Parallel()

	echo := startEcho(t)
	_, c := dialTestServer(t)

	tun, err := c.Forward(context.Background(), "127.0.0.1:0", echo.Addr().String(), nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}

	// Bytes must survive the whole path: host listener -> ssh direct-tcpip ->
	// echo listener and back.
	conn, err := net.Dial("tcp", tun.Addr().String())
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()

	const msg = "plastic turtle\n"
	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("round trip: got %q want %q", buf, msg)
	}

	// A second connection is left idle so that Close has something in flight
	// to tear down; a Close that only stopped the listener would leave it open.
	idle, err := net.Dial("tcp", tun.Addr().String())
	if err != nil {
		t.Fatalf("dial tunnel (idle): %v", err)
	}
	defer idle.Close()
	// Make sure the tunnel has actually accepted and registered it before
	// closing, otherwise the assertion below could pass for the wrong reason.
	if _, err := io.WriteString(idle, "x"); err != nil {
		t.Fatalf("write idle: %v", err)
	}
	one := make([]byte, 1)
	_ = idle.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(idle, one); err != nil {
		t.Fatalf("read idle: %v", err)
	}

	addr := tun.Addr().String()
	if err := tun.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if probe, err := net.Dial("tcp", addr); err == nil {
		probe.Close()
		t.Fatalf("listener still accepting at %s after Close", addr)
	}

	// EOF or a reset both mean the pipe was torn down; a timeout means Close
	// stopped the listener and forgot the connections behind it.
	_ = idle.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := idle.Read(one)
	if err == nil {
		t.Fatalf("in-flight connection still open after Close (read %d bytes)", n)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("in-flight connection neither closed nor reset: %v", err)
	}
}

// TestForwardForwardsHalfClose pins the behaviour a request/response protocol
// depends on: closing the write half must reach the guest as EOF, and the
// reply must still come back afterwards.
func TestForwardForwardsHalfClose(t *testing.T) {
	t.Parallel()

	echo := startEcho(t)
	_, c := dialTestServer(t)

	tun, err := c.Forward(context.Background(), "127.0.0.1:0", echo.Addr().String(), nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer tun.Close()

	conn, err := net.Dial("tcp", tun.Addr().String())
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()

	const msg = "request"
	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half close: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read after half close: %v", err)
	}
	if string(got) != msg {
		t.Fatalf("reply after half close = %q, want %q", got, msg)
	}
}

func TestForwardRejectsNonLoopback(t *testing.T) {
	t.Parallel()

	_, c := dialTestServer(t)
	echo := startEcho(t)

	for _, addr := range []string{
		"0.0.0.0:0",
		":0",
		"[::]:0",
		"192.0.2.1:0",
		"example.invalid:0",
	} {
		tun, err := c.Forward(context.Background(), addr, echo.Addr().String(), nil)
		if err == nil {
			_ = tun.Close()
			t.Errorf("Forward(%q) succeeded; want rejection", addr)
		}
	}

	// The loopback forms must still work, or the check is useless in practice.
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		tun, err := c.Forward(context.Background(), addr, echo.Addr().String(), nil)
		if err != nil {
			t.Errorf("Forward(%q) rejected: %v", addr, err)
			continue
		}
		_ = tun.Close()
	}
}

func TestTunnelCloseIsIdempotentUnderConcurrency(t *testing.T) {
	t.Parallel()

	echo := startEcho(t)
	_, c := dialTestServer(t)

	tun, err := c.Forward(context.Background(), "127.0.0.1:0", echo.Addr().String(), nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}

	// Keep a connection alive across the close storm so the teardown has to
	// walk the conn set while another goroutine may be removing from it.
	conn, err := net.Dial("tcp", tun.Addr().String())
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := tun.Close(); err != nil {
				t.Errorf("concurrent Close: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
}

func TestClientCloseClosesTunnels(t *testing.T) {
	t.Parallel()

	echo := startEcho(t)
	_, c := dialTestServer(t)

	tun, err := c.Forward(context.Background(), "127.0.0.1:0", echo.Addr().String(), nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	addr := tun.Addr().String()

	if err := c.Close(); err != nil {
		t.Fatalf("Client.Close: %v", err)
	}
	if probe, err := net.Dial("tcp", addr); err == nil {
		probe.Close()
		t.Fatalf("tunnel listener survived Client.Close")
	}
	// Idempotent: the supervisor's teardown and a deferred close both reach it.
	if err := c.Close(); err != nil {
		t.Fatalf("second Client.Close: %v", err)
	}
	if _, err := c.Forward(context.Background(), "127.0.0.1:0", echo.Addr().String(), nil); err == nil {
		t.Fatalf("Forward on a closed client succeeded")
	}
}

func TestForwardStopsWhenContextIsCancelled(t *testing.T) {
	t.Parallel()

	echo := startEcho(t)
	_, c := dialTestServer(t)

	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	if tun, err := c.Forward(dead, "127.0.0.1:0", echo.Addr().String(), nil); err == nil {
		_ = tun.Close()
		t.Error("Forward with a cancelled context succeeded")
	}

	ctx, cancel := context.WithCancel(context.Background())
	tun, err := c.Forward(ctx, "127.0.0.1:0", echo.Addr().String(), nil)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	addr := tun.Addr().String()
	cancel()

	// The listener goes away asynchronously; poll briefly rather than assume
	// the callback has already run.
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, err := net.Dial("tcp", addr)
		if err != nil {
			return
		}
		probe.Close()
		if time.Now().After(deadline) {
			t.Fatalf("tunnel listener survived context cancellation")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
