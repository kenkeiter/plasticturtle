package sshx

import (
	"bytes"
	"strings"
	"testing"
)

// testBar returns a statusBar writing to buf, with a render function whose
// output is easy to spot in the byte stream.
func testBar(buf *bytes.Buffer, width, height int) *statusBar {
	return &statusBar{
		out:    buf,
		render: func(w int) string { return "BANNER" },
		width:  width,
		height: height,
	}
}

func TestFilterPassesPlainOutputThrough(t *testing.T) {
	var buf bytes.Buffer
	f := &vtFilter{bar: testBar(&buf, 80, 24)}

	const text = "hello, guest\r\nmore output"
	n, err := f.Write([]byte(text))
	if err != nil || n != len(text) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(text))
	}
	if buf.String() != text {
		t.Fatalf("output %q, want %q", buf.String(), text)
	}
}

func TestFilterRewritesBareRegionReset(t *testing.T) {
	var buf bytes.Buffer
	f := &vtFilter{bar: testBar(&buf, 80, 24)}

	if _, err := f.Write([]byte("a\x1b[rb")); err != nil {
		t.Fatal(err)
	}
	// The guest's "whole screen" is rows 1..23; a bare reset would have
	// reclaimed row 24.
	if got, want := buf.String(), "a\x1b[1;23rb"; got != want {
		t.Fatalf("output %q, want %q", got, want)
	}
}

func TestFilterLeavesParameterizedRegionAlone(t *testing.T) {
	var buf bytes.Buffer
	f := &vtFilter{bar: testBar(&buf, 80, 24)}

	const seq = "\x1b[5;20r"
	if _, err := f.Write([]byte(seq)); err != nil {
		t.Fatal(err)
	}
	if buf.String() != seq {
		t.Fatalf("output %q, want %q", buf.String(), seq)
	}
}

func TestFilterRepaintsAfterClear(t *testing.T) {
	for _, seq := range []string{"\x1b[2J", "\x1b[J", "\x1b[0J", "\x1b[3J"} {
		var buf bytes.Buffer
		f := &vtFilter{bar: testBar(&buf, 80, 24)}
		if _, err := f.Write([]byte(seq)); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if !strings.HasPrefix(out, seq) {
			t.Errorf("%q: clear not passed through, got %q", seq, out)
		}
		if !strings.Contains(out, "BANNER") {
			t.Errorf("%q: no repaint after clear, got %q", seq, out)
		}
	}
}

func TestFilterDoesNotRepaintAfterEraseAbove(t *testing.T) {
	// ED 1 erases from the top of the screen to the cursor; it cannot touch
	// the bottom row, so repainting would be wasted bytes on every prompt
	// that uses it.
	var buf bytes.Buffer
	f := &vtFilter{bar: testBar(&buf, 80, 24)}
	if _, err := f.Write([]byte("\x1b[1J")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "BANNER") {
		t.Fatalf("unwanted repaint: %q", buf.String())
	}
}

func TestFilterReassertsAfterAltScreenAndReset(t *testing.T) {
	for _, seq := range []string{"\x1b[?1049h", "\x1b[?1049l", "\x1b[?47h", "\x1b[?1047l", "\x1bc", "\x1b[!p"} {
		var buf bytes.Buffer
		f := &vtFilter{bar: testBar(&buf, 80, 24)}
		if _, err := f.Write([]byte(seq)); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if !strings.HasPrefix(out, seq) {
			t.Errorf("%q: sequence not passed through, got %q", seq, out)
		}
		if !strings.Contains(out, "\x1b[1;23r") {
			t.Errorf("%q: region not re-asserted, got %q", seq, out)
		}
		if !strings.Contains(out, "BANNER") {
			t.Errorf("%q: no repaint, got %q", seq, out)
		}
	}
}

func TestFilterHoldsPartialSequenceAcrossWrites(t *testing.T) {
	var buf bytes.Buffer
	f := &vtFilter{bar: testBar(&buf, 80, 24)}

	// A bare region reset split at every possible byte boundary must still be
	// rewritten, never leak through in pieces.
	if _, err := f.Write([]byte("x\x1b")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "x" {
		t.Fatalf("partial escape leaked: %q", got)
	}
	if _, err := f.Write([]byte("[")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("ry")); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "x\x1b[1;23ry"; got != want {
		t.Fatalf("output %q, want %q", got, want)
	}
}

func TestFilterPassesOSCWithoutMisparsing(t *testing.T) {
	var buf bytes.Buffer
	f := &vtFilter{bar: testBar(&buf, 80, 24)}

	// The title payload contains bytes that look like a clear; they are OSC
	// data and must trigger nothing.
	const seq = "\x1b]0;title [2J not a clear\x07after"
	if _, err := f.Write([]byte(seq)); err != nil {
		t.Fatal(err)
	}
	if buf.String() != seq {
		t.Fatalf("output %q, want %q", buf.String(), seq)
	}
}

func TestFilterPassesOSCTerminatedByST(t *testing.T) {
	var buf bytes.Buffer
	f := &vtFilter{bar: testBar(&buf, 80, 24)}

	const seq = "\x1b]52;c;aGVsbG8=\x1b\\after"
	if _, err := f.Write([]byte(seq)); err != nil {
		t.Fatal(err)
	}
	if buf.String() != seq {
		t.Fatalf("output %q, want %q", buf.String(), seq)
	}
}

func TestResizeReportsGuestHeightAndRepaints(t *testing.T) {
	var buf bytes.Buffer
	b := testBar(&buf, 80, 24)

	if got := b.resize(100, 40); got != 39 {
		t.Fatalf("resize guest height = %d, want 39", got)
	}
	out := buf.String()
	if !strings.Contains(out, "\x1b[1;39r") {
		t.Errorf("region not re-anchored: %q", out)
	}
	if !strings.Contains(out, "\x1b[40;1H") {
		t.Errorf("banner not painted on new bottom row: %q", out)
	}
}

func TestRefreshIsNoOpWhenDetached(t *testing.T) {
	s := &StatusLine{Render: func(int) string { return "X" }}
	s.Refresh() // must not panic

	var buf bytes.Buffer
	bar := testBar(&buf, 80, 24)
	s.attach(bar)
	s.Refresh()
	if !strings.Contains(buf.String(), "BANNER") {
		t.Fatalf("attached Refresh did not paint: %q", buf.String())
	}
	s.detach()
	buf.Reset()
	s.Refresh()
	if buf.Len() != 0 {
		t.Fatalf("detached Refresh wrote %q", buf.String())
	}
}

func TestStartAndStopBracketTheRow(t *testing.T) {
	var buf bytes.Buffer
	b := testBar(&buf, 80, 24)

	b.start()
	out := buf.String()
	for _, want := range []string{"\x1b[1;23r", "\x1b[24;1H", "BANNER"} {
		if !strings.Contains(out, want) {
			t.Errorf("start missing %q in %q", want, out)
		}
	}

	buf.Reset()
	b.stop()
	out = buf.String()
	for _, want := range []string{"\x1b[r", "\x1b[2K"} {
		if !strings.Contains(out, want) {
			t.Errorf("stop missing %q in %q", want, out)
		}
	}
}
