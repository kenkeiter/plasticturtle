package main

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/kenkeiter/plasticturtle/internal/netfw"
)

// fakeLink is an in-memory frameConn: it hands out queued frames one per
// ReadFrame (modeling the message boundaries both real ends preserve), records
// what is written to it, and can be closed mid-read the way vmnetlink.Close
// unblocks a parked reader.
type fakeLink struct {
	maxPacket int

	mu       sync.Mutex
	in       [][]byte
	out      [][]byte
	blocked  chan struct{} // closed by close(); nil means read returns EOF when drained
	writeErr error
}

func newFakeLink(frames ...[]byte) *fakeLink {
	return &fakeLink{maxPacket: 1514, in: frames}
}

// blocking makes the link park after its queued frames instead of reporting
// EOF, so a test can end it with close() and observe the shutdown path.
func (f *fakeLink) blocking() *fakeLink {
	f.blocked = make(chan struct{})
	return f
}

func (f *fakeLink) ReadFrame(buf []byte) (int, error) {
	f.mu.Lock()
	if len(f.in) > 0 {
		frame := f.in[0]
		f.in = f.in[1:]
		f.mu.Unlock()
		if len(frame) > len(buf) {
			return 0, errors.New("frame does not fit in buffer")
		}
		return copy(buf, frame), nil
	}
	blocked := f.blocked
	f.mu.Unlock()
	if blocked == nil {
		return 0, io.EOF
	}
	<-blocked
	return 0, net.ErrClosed
}

func (f *fakeLink) WriteFrame(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.out = append(f.out, append([]byte(nil), p...))
	return nil
}

func (f *fakeLink) MaxPacketSize() int { return f.maxPacket }

func (f *fakeLink) close() { close(f.blocked) }

func (f *fakeLink) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var got []string
	for _, p := range f.out {
		got = append(got, string(p))
	}
	return got
}

func framesEqual(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("frames = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("frames = %q, want %q", got, want)
		}
	}
}

func TestRelayPassThrough(t *testing.T) {
	src := newFakeLink([]byte("frame-a"), []byte("frame-b"))
	dst := newFakeLink()
	var st stats
	if err := relayFiltered(dst, src, nil, &st); err != nil {
		t.Fatal(err)
	}
	framesEqual(t, dst.written(), "frame-a", "frame-b")
	if st.passed.Load() != 2 || st.dropped.Load() != 0 {
		t.Fatalf("stats passed=%d dropped=%d", st.passed.Load(), st.dropped.Load())
	}
}

// TestRelayEgressVerdicts drives the guest->world direction: drops stay dropped
// and pass-through frames arrive unchanged.
func TestRelayEgressVerdicts(t *testing.T) {
	src := newFakeLink([]byte("keep"), []byte("drop"), []byte("keep2"))
	dst := newFakeLink()
	var st stats
	decide := func(f []byte) netfw.Verdict {
		if string(f) == "drop" {
			return netfw.Verdict{Action: netfw.Drop}
		}
		return netfw.Verdict{Action: netfw.Pass}
	}
	if err := relayFiltered(dst, src, decide, &st); err != nil {
		t.Fatal(err)
	}
	framesEqual(t, dst.written(), "keep", "keep2")
	if st.passed.Load() != 2 || st.dropped.Load() != 1 {
		t.Fatalf("stats passed=%d dropped=%d", st.passed.Load(), st.dropped.Load())
	}
}

// TestRelayIngressReplaceGoesToGuest is the NXDOMAIN path: netfw replaces a
// disallowed DNS answer on the world->guest direction, and the substitute must
// continue toward the guest in place of the original, not back out.
func TestRelayIngressReplaceGoesToGuest(t *testing.T) {
	world := newFakeLink([]byte("dns-answer"), []byte("other"))
	guest := newFakeLink()
	var st stats
	decide := func(f []byte) netfw.Verdict {
		if string(f) == "dns-answer" {
			return netfw.Verdict{Action: netfw.Replace, ReplacementFrame: []byte("NXDOMAIN")}
		}
		return netfw.Verdict{Action: netfw.Pass}
	}
	if err := relayFiltered(guest, world, decide, &st); err != nil {
		t.Fatal(err)
	}
	framesEqual(t, guest.written(), "NXDOMAIN", "other")
	if st.rewrote.Load() != 1 || st.passed.Load() != 1 || st.dropped.Load() != 0 {
		t.Fatalf("stats passed=%d dropped=%d rewrote=%d", st.passed.Load(), st.dropped.Load(), st.rewrote.Load())
	}
}

// TestRelayReadBufferCoversLink asserts the read buffer follows the link's own
// maximum when that exceeds the datagram-side constant, so a jumbo frame is
// never rejected for not fitting.
func TestRelayReadBufferCoversLink(t *testing.T) {
	big := make([]byte, maxFrame+1024)
	for i := range big {
		big[i] = 'x'
	}
	src := newFakeLink(big)
	src.maxPacket = len(big)
	dst := newFakeLink()
	var st stats
	if err := relayFiltered(dst, src, nil, &st); err != nil {
		t.Fatal(err)
	}
	if got := dst.written(); len(got) != 1 || len(got[0]) != len(big) {
		t.Fatalf("oversized frame not relayed intact (%d frames)", len(got))
	}
}

// ethFrame builds a minimal ethernet frame with the given source MAC.
func ethFrame(src net.HardwareAddr, payload string) []byte {
	f := make([]byte, ethHeaderLen)
	copy(f[6:12], src)
	f[12], f[13] = 0x08, 0x00 // IPv4
	return append(f, payload...)
}

func TestMACEnforcementDropsForeignSource(t *testing.T) {
	guestMAC, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	otherMAC, _ := net.ParseMAC("11:22:33:44:55:66")
	src := newFakeLink(
		ethFrame(guestMAC, "mine"),
		ethFrame(otherMAC, "spoofed"),
		[]byte("runt"), // no ethernet header at all
	)
	dst := newFakeLink()
	var st stats
	decide := enforceSourceMAC(guestMAC, nil, &st, newLogger(""))
	if err := relayFiltered(dst, src, decide, &st); err != nil {
		t.Fatal(err)
	}
	framesEqual(t, dst.written(), string(ethFrame(guestMAC, "mine")))
	if st.macDropped.Load() != 2 || st.dropped.Load() != 2 || st.passed.Load() != 1 {
		t.Fatalf("stats passed=%d dropped=%d mac-dropped=%d",
			st.passed.Load(), st.dropped.Load(), st.macDropped.Load())
	}
}

// TestMACEnforcementDefersToFilter checks the guard is a wrapper, not a
// replacement: a frame with the right MAC still faces the policy verdict.
func TestMACEnforcementDefersToFilter(t *testing.T) {
	guestMAC, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	var st stats
	inner := func([]byte) netfw.Verdict { return netfw.Verdict{Action: netfw.Drop} }
	decide := enforceSourceMAC(guestMAC, inner, &st, newLogger(""))
	if v := decide(ethFrame(guestMAC, "payload")); v.Action != netfw.Drop {
		t.Fatalf("inner verdict not applied: %v", v.Action)
	}
	if st.macDropped.Load() != 0 {
		t.Fatalf("policy drop miscounted as a MAC drop (%d)", st.macDropped.Load())
	}
}

// TestRelayCleanShutdownOnClose models the real stop: both directions are
// parked, the link closes, and neither direction reports an error.
func TestRelayCleanShutdownOnClose(t *testing.T) {
	link := newFakeLink().blocking()
	guest := newFakeLink().blocking()
	var egress, ingress stats

	errc := make(chan error, 2)
	go func() { errc <- relayFiltered(link, guest, nil, &egress) }()
	go func() { errc <- relayFiltered(guest, link, nil, &ingress) }()

	link.close()
	guest.close()
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil {
			t.Fatalf("relay %d ended with %v, want clean shutdown", i, err)
		}
	}
}

// TestRelayEOFIsCleanShutdown covers Tart exiting: stdin reaches EOF and the
// direction reading it ends without an error.
func TestRelayEOFIsCleanShutdown(t *testing.T) {
	var st stats
	if err := relayFiltered(newFakeLink(), newFakeLink(), nil, &st); err != nil {
		t.Fatalf("EOF reported as error: %v", err)
	}
}

func TestRelayReportsRealErrors(t *testing.T) {
	dst := newFakeLink()
	dst.writeErr = errors.New("boom")
	var st stats
	if err := relayFiltered(dst, newFakeLink([]byte("frame")), nil, &st); err == nil {
		t.Fatal("write failure was swallowed")
	}
}
