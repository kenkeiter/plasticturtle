package sshx

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// ForwardAny must reach a guest service that only starts listening AFTER the
// tunnel is up, on the second of its candidate addresses.
//
// This is the case the previous implementation could not serve. It probed the
// candidates once at tunnel-setup time, from the host, seconds after boot —
// when nothing is listening on any of them — so the probe always answered "no"
// and the fallback was dead code. The question only has a true answer when a
// real connection asks it.
func TestForwardAnyFallsBackToALaterCandidate(t *testing.T) {
	srv, err := NewTestServer(Credentials{User: "admin", Password: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c, err := Dial(context.Background(), srv.Addr(), Credentials{User: "admin", Password: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Candidate 1 is a port nothing will ever answer on; candidate 2 is where
	// the service eventually appears.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := deadLn.Addr().String()
	_ = deadLn.Close() // now refusing

	liveLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer liveLn.Close()
	go func() {
		for {
			conn, err := liveLn.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("served\n"))
			_ = conn.Close()
		}
	}()

	var logged strings.Builder
	tun, err := c.ForwardAny(context.Background(), "127.0.0.1:0",
		[]string{deadAddr, liveLn.Addr().String()},
		func(f string, a ...any) { fmt.Fprintf(&logged, f+"\n", a...) })
	if err != nil {
		t.Fatalf("ForwardAny: %v", err)
	}
	defer tun.Close()

	read := func() string {
		conn, err := net.DialTimeout("tcp", tun.Addr().String(), 3*time.Second)
		if err != nil {
			t.Fatalf("dial the forward: %v", err)
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		return string(buf[:n])
	}

	if got := read(); !strings.Contains(got, "served") {
		t.Fatalf("first connection read %q; the fallback candidate was not tried", got)
	}
	// The winner is latched, so the dead candidate is not retried on every
	// subsequent connection.
	if got := read(); !strings.Contains(got, "served") {
		t.Errorf("second connection read %q", got)
	}
	if !strings.Contains(logged.String(), liveLn.Addr().String()) {
		t.Errorf("the chosen guest address was not reported:\n%s", logged.String())
	}
}

// A single candidate behaves exactly as Forward always has.
func TestForwardAnyRejectsAnEmptyCandidateList(t *testing.T) {
	srv, err := NewTestServer(Credentials{User: "admin", Password: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c, err := Dial(context.Background(), srv.Addr(), Credentials{User: "admin", Password: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.ForwardAny(context.Background(), "127.0.0.1:0", nil, nil); err == nil {
		t.Error("ForwardAny accepted a forward with nowhere to dial")
	}
}
