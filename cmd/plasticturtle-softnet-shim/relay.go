package main

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/kenkeiter/plasticturtle/internal/netfw"
)

// maxFrame is the largest datagram the relay will handle on the Tart side. vmnet
// MTUs are ~1500; jumbo would still be well under this, and a SOCK_DGRAM read is
// all-or-nothing so an oversized frame is truncated by the kernel, not by us.
const maxFrame = 65536

// ethHeaderLen is dst MAC (6) + src MAC (6) + ethertype (2). A frame shorter
// than this has no source address to check.
const ethHeaderLen = 14

// frameConn is one end of the relay: a carrier of whole ethernet frames, with
// message boundaries preserved in both directions. Both ends of this relay are
// frame-oriented but nothing else alike — one is the SOCK_DGRAM socket Tart
// handed us as stdin, the other a vmnet interface — so the relay talks to them
// through this seam, which also lets tests drive both ends in memory.
type frameConn interface {
	// ReadFrame blocks until one frame arrives and copies it into buf.
	ReadFrame(buf []byte) (int, error)
	// WriteFrame writes one frame whole.
	WriteFrame(p []byte) error
	// MaxPacketSize is the largest frame ReadFrame may deliver, i.e. the read
	// buffer this end needs.
	MaxPacketSize() int
}

// datagramLink adapts the SOCK_DGRAM socket Tart passes as stdin to frameConn.
// One datagram is one ethernet frame, so a read and a frame are the same thing.
type datagramLink struct{ f *os.File }

func (d datagramLink) ReadFrame(buf []byte) (int, error) { return d.f.Read(buf) }

func (d datagramLink) WriteFrame(p []byte) error {
	_, err := d.f.Write(p)
	return err
}

func (d datagramLink) MaxPacketSize() int { return maxFrame }

// verdictFunc is one direction's filter entry point.
type verdictFunc func([]byte) netfw.Verdict

// stats counts what a relay direction did, for periodic logging.
type stats struct {
	frames     atomic.Int64
	passed     atomic.Int64
	dropped    atomic.Int64
	rewrote    atomic.Int64
	macDropped atomic.Int64 // subset of dropped: wrong source MAC
}

// relayFiltered copies frames from src to dst, applying decide to each. A
// Replace verdict forwards the substitute frame in the same direction as the
// original — netfw's only Replace is the NXDOMAIN answer that stands in for a
// disallowed DNS response, which is already travelling toward the guest.
//
// It returns when src closes (nil) or either side errors. A nil decide is a pure
// pass-through (used for the open policy), which keeps the fast path free of the
// filter entirely.
func relayFiltered(dst, src frameConn, decide verdictFunc, st *stats) error {
	buf := make([]byte, max(src.MaxPacketSize(), maxFrame))
	for {
		n, err := src.ReadFrame(buf)
		if n > 0 {
			frame := buf[:n]
			st.frames.Add(1)
			out := frame
			if decide != nil {
				switch v := decide(frame); v.Action {
				case netfw.Drop:
					st.dropped.Add(1)
					continue
				case netfw.Replace:
					st.rewrote.Add(1)
					out = v.ReplacementFrame
				default:
					st.passed.Add(1)
				}
			} else {
				st.passed.Add(1)
			}
			if werr := dst.WriteFrame(out); werr != nil {
				if isShutdown(werr) {
					return nil
				}
				return werr
			}
		}
		if err != nil {
			if isShutdown(err) {
				return nil
			}
			return err
		}
	}
}

// isShutdown reports whether an I/O error is the normal end of the relay rather
// than a fault: EOF when Tart closes the guest link, and net.ErrClosed (which
// vmnetlink returns, and which os.File reports under its own name) when the
// interface is closed out from under a parked read.
func isShutdown(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed)
}

// enforceSourceMAC wraps an egress verdict function with the check softnet made
// via --vm-mac-address: a frame from the guest must carry the guest's own source
// MAC. Anything else is a guest trying to speak as another station on the link,
// and the pin table's per-flow reasoning does not apply to it.
//
// Only the first offender is logged. The check runs on attacker-chosen input, so
// a hostile guest could otherwise turn it into a log-flooding primitive.
func enforceSourceMAC(want net.HardwareAddr, next verdictFunc, st *stats, lg *logger) verdictFunc {
	var once sync.Once
	return func(frame []byte) netfw.Verdict {
		if !hasSourceMAC(frame, want) {
			st.macDropped.Add(1)
			once.Do(func() {
				lg.printf("DENY egress from source MAC %s (guest is %s); further occurrences not logged",
					sourceMAC(frame), want)
			})
			return netfw.Verdict{Action: netfw.Drop}
		}
		if next == nil {
			return netfw.Verdict{Action: netfw.Pass}
		}
		return next(frame)
	}
}

// hasSourceMAC reports whether frame's ethernet source address is want. A runt
// with no complete header fails, like every other unparsable frame.
func hasSourceMAC(frame []byte, want net.HardwareAddr) bool {
	if len(frame) < ethHeaderLen || len(want) != 6 {
		return false
	}
	return string(frame[6:12]) == string(want)
}

// sourceMAC renders a frame's source address for a log line, tolerating a runt.
func sourceMAC(frame []byte) string {
	if len(frame) < ethHeaderLen {
		return "<short frame>"
	}
	return net.HardwareAddr(frame[6:12]).String()
}
