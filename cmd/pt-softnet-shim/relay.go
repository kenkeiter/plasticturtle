package main

import (
	"io"
	"sync/atomic"

	"github.com/kenkeiter/plasticturtle/internal/netfw"
)

// maxFrame is the largest datagram the relay will handle. vmnet MTUs are ~1500;
// jumbo would still be well under this, and a SOCK_DGRAM read is all-or-nothing
// so an oversized frame is truncated by the kernel, not by us.
const maxFrame = 65536

// verdictFunc is one direction's filter entry point.
type verdictFunc func([]byte) netfw.Verdict

// stats counts what a relay direction did, for periodic logging.
type stats struct {
	frames  atomic.Int64
	passed  atomic.Int64
	dropped atomic.Int64
	rewrote atomic.Int64
}

// relayFiltered copies datagrams from src to dst, applying decide to each. It
// returns when src reaches EOF or either side errors. A nil decide is a pure
// pass-through (used for the open policy), which keeps the fast path free of the
// filter entirely.
func relayFiltered(dst io.Writer, src io.Reader, decide verdictFunc, st *stats) error {
	buf := make([]byte, maxFrame)
	for {
		n, err := src.Read(buf)
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
			if _, werr := dst.Write(out); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
