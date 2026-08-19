package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/kenkeiter/plasticturtle/internal/netfw"
)

// chunkReader yields each frame as a distinct Read, modeling SOCK_DGRAM message
// boundaries, then EOF.
type chunkReader struct {
	frames [][]byte
	i      int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.i >= len(c.frames) {
		return 0, io.EOF
	}
	n := copy(p, c.frames[c.i])
	c.i++
	return n, nil
}

func TestRelayPassThrough(t *testing.T) {
	in := [][]byte{[]byte("frame-a"), []byte("frame-b")}
	var out bytes.Buffer
	var st stats
	if err := relayFiltered(&out, &chunkReader{frames: in}, nil, &st); err != nil {
		t.Fatal(err)
	}
	if out.String() != "frame-aframe-b" {
		t.Fatalf("pass-through output = %q", out.String())
	}
	if st.passed.Load() != 2 || st.dropped.Load() != 0 {
		t.Fatalf("stats passed=%d dropped=%d", st.passed.Load(), st.dropped.Load())
	}
}

func TestRelayAppliesVerdicts(t *testing.T) {
	in := [][]byte{[]byte("keep"), []byte("drop"), []byte("swap")}
	var out bytes.Buffer
	var st stats
	decide := func(f []byte) netfw.Verdict {
		switch string(f) {
		case "drop":
			return netfw.Verdict{Action: netfw.Drop}
		case "swap":
			return netfw.Verdict{Action: netfw.Replace, ReplacementFrame: []byte("REPL")}
		default:
			return netfw.Verdict{Action: netfw.Pass}
		}
	}
	if err := relayFiltered(&out, &chunkReader{frames: in}, decide, &st); err != nil {
		t.Fatal(err)
	}
	if out.String() != "keepREPL" {
		t.Fatalf("filtered output = %q, want keepREPL", out.String())
	}
	if st.passed.Load() != 1 || st.dropped.Load() != 1 || st.rewrote.Load() != 1 {
		t.Fatalf("stats passed=%d dropped=%d rewrote=%d", st.passed.Load(), st.dropped.Load(), st.rewrote.Load())
	}
}
