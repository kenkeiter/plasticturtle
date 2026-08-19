package sshx

import (
	"fmt"
	"io"
	"sync"
)

// StatusLine is a single reserved row at the bottom of the local terminal,
// kept out of the guest's reach for the lifetime of an interactive session.
//
// The mechanism has three parts, all of which must agree or the display
// corrupts: the guest's PTY is told the terminal is one row shorter than it
// is, a scroll region (DECSTBM) confines scrolling to the rows above, and the
// guest's output is filtered for the few sequences that would still reach the
// reserved row — full-screen clears, scroll-region resets, terminal resets and
// alternate-screen switches — each of which is followed by a repaint.
type StatusLine struct {
	// Render returns the styled content for the row at the given width. It
	// must occupy at most width terminal columns — one more wraps the cursor
	// into the guest's rows, which corrupts the display — and must not move
	// the cursor (SGR styling is fine, CUP/LF are not). It is called with the
	// status bar's lock held, so it must be fast and must not touch the tty.
	Render func(width int) string

	mu  sync.Mutex
	bar *statusBar
}

// Refresh repaints the row with fresh Render output. It is safe from any
// goroutine and is a no-op when no interactive session is showing the line.
func (s *StatusLine) Refresh() {
	s.mu.Lock()
	bar := s.bar
	s.mu.Unlock()
	if bar != nil {
		bar.repaint()
	}
}

func (s *StatusLine) attach(b *statusBar) {
	s.mu.Lock()
	s.bar = b
	s.mu.Unlock()
}

func (s *StatusLine) detach() {
	s.mu.Lock()
	s.bar = nil
	s.mu.Unlock()
}

// statusBar owns the reserved row for one session. Its mutex serializes the
// three writers of the terminal: the guest-output filter, Refresh calls from
// other goroutines, and the resize handler.
type statusBar struct {
	mu     sync.Mutex
	out    io.Writer
	render func(int) string
	width  int
	height int // the real terminal height; the guest is told height-1
}

// guestHeight is the height reported to the guest: everything above the
// reserved row.
func (b *statusBar) guestHeight() int {
	if b.height <= 1 {
		return 1
	}
	return b.height - 1
}

// start reserves the bottom row and paints it.
//
// The line feed at the real bottom first scrolls the screen up one row, so
// whatever the cursor was on — typically the pt shell command line — is not
// buried under the banner. Setting the region homes the cursor (DECSTBM's
// defined side effect), so the guest's first output is then anchored at the
// bottom of its area, where a fresh prompt reads naturally.
func (b *statusBar) start() {
	b.mu.Lock()
	defer b.mu.Unlock()
	fmt.Fprintf(b.out, "\x1b[%d;1H\n", b.height)
	fmt.Fprintf(b.out, "\x1b[1;%dr\x1b[%d;1H", b.guestHeight(), b.guestHeight())
	b.paintLocked()
}

// stop returns the row to the terminal: region reset, banner erased, cursor
// left where the guest's last output put it. It must run while the terminal
// is still in raw mode, i.e. before Interactive's term.Restore.
func (b *statusBar) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	fmt.Fprintf(b.out, "\x1b[0m\x1b7\x1b[r\x1b[%d;1H\x1b[2K\x1b8", b.height)
}

// resize re-anchors the region and banner to the new geometry and returns the
// height the guest should be told. On a shrink the guest's cursor may sit on
// the reserved row for a moment; its own WINCH redraw moves it off.
func (b *statusBar) resize(width, height int) (guestHeight int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.width, b.height = width, height
	b.reassertLocked()
	b.paintLocked()
	return b.guestHeight()
}

func (b *statusBar) repaint() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paintLocked()
}

// reassertLocked re-establishes the scroll region without disturbing the
// cursor: DECSTBM homes it, so the save/restore pair around it is mandatory.
func (b *statusBar) reassertLocked() {
	fmt.Fprintf(b.out, "\x1b7\x1b[1;%dr\x1b8", b.guestHeight())
}

// paintLocked draws the row. Callers hold b.mu.
//
// DECSC/DECRC (ESC 7 / ESC 8) bracket the draw because they save and restore
// position, SGR attributes and origin mode together; origin mode is cleared
// in between so the row address means the real screen even if the guest
// enabled DECOM. The known cost: a repaint landing between a guest's own
// DECSC and DECRC clobbers its saved slot. Modern full-screen programs use
// the 1049 alternate screen instead, so the window is accepted.
func (b *statusBar) paintLocked() {
	if b.height < 2 {
		return
	}
	content := ""
	if b.render != nil {
		content = b.render(b.width)
	}
	fmt.Fprintf(b.out, "\x1b7\x1b[?6l\x1b[%d;1H\x1b[0m%s\x1b[0m\x1b8", b.height, content)
}

// maxHeldEscape bounds how much of an unfinished control sequence the filter
// holds back between writes. OSC payloads (titles, clipboard) can be long;
// anything longer than this is passed through unparsed rather than delaying
// real output, at worst costing one spurious repaint.
const maxHeldEscape = 4096

// vtFilter forwards guest output to the terminal, rewriting or reacting to
// the sequences that would breach the reserved row. It takes the bar's lock
// for each Write, which is what keeps guest output and banner repaints from
// interleaving mid-sequence.
type vtFilter struct {
	bar  *statusBar
	held []byte // unfinished escape sequence from the previous Write
}

// escape-sequence classifications for the filter.
type escKind int

const (
	escOther       escKind = iota // complete, no action
	escRegionReset                // CSI r with no parameters: rewrite
	escClear                      // erases that may include the reserved row
	escReset                      // RIS / DECSTR: region and banner both gone
	escAltScreen                  // alternate screen enter or exit
)

func (f *vtFilter) Write(p []byte) (int, error) {
	f.bar.mu.Lock()
	defer f.bar.mu.Unlock()

	data := p
	if len(f.held) > 0 {
		data = append(f.held, p...)
		f.held = nil
	}

	var repaint, reassert bool
	emitted := 0 // data[:emitted] has been written out
	var werr error
	emit := func(b []byte) {
		if werr == nil && len(b) > 0 {
			_, werr = f.bar.out.Write(b)
		}
	}

	for i := 0; i < len(data); {
		if data[i] != 0x1b {
			i++
			continue
		}
		n, kind, complete := parseEscape(data[i:])
		if !complete {
			if len(data)-i <= maxHeldEscape {
				emit(data[emitted:i])
				f.held = append(f.held, data[i:]...)
				emitted = len(data)
				break
			}
			// Too long to be anything this filter understands; let the
			// terminal sort it out.
			i++
			continue
		}
		switch kind {
		case escRegionReset:
			// A bare CSI r means "scroll region = my whole screen", and the
			// guest's whole screen is the rows above the banner. Rewriting
			// preserves the sequence's other effect (homing the cursor)
			// because DECSTBM homes regardless of its parameters.
			emit(data[emitted:i])
			emit(fmt.Appendf(nil, "\x1b[1;%dr", f.bar.guestHeight()))
			emitted = i + n
		case escClear:
			repaint = true
		case escReset:
			reassert, repaint = true, true
		case escAltScreen:
			// xterm keeps DECSTBM margins across the switch, but not every
			// terminal does; re-asserting is idempotent where they do.
			reassert, repaint = true, true
		}
		i += n
	}
	emit(data[emitted:])

	if reassert {
		f.bar.reassertLocked()
	}
	if repaint {
		f.bar.paintLocked()
	}
	if werr != nil {
		return 0, werr
	}
	return len(p), nil
}

// parseEscape classifies the escape sequence at the start of data (data[0]
// must be ESC). It returns the sequence's length and kind, or complete=false
// when data ends before the sequence does.
func parseEscape(data []byte) (n int, kind escKind, complete bool) {
	if len(data) < 2 {
		return 0, escOther, false
	}
	switch data[1] {
	case '[':
		return parseCSI(data)
	case ']', 'P', '^', '_':
		// OSC/DCS/PM/APC: a string terminated by BEL (OSC convention) or ST
		// (ESC \). None affect the reserved row; the parse exists so their
		// payload bytes are never mistaken for sequences.
		for i := 2; i < len(data); i++ {
			if data[i] == 0x07 {
				return i + 1, escOther, true
			}
			if data[i] == 0x1b {
				if i+1 >= len(data) {
					return 0, escOther, false
				}
				if data[i+1] == '\\' {
					return i + 2, escOther, true
				}
			}
		}
		return 0, escOther, false
	case 'c':
		return 2, escReset, true
	case '(', ')', '*', '+':
		// Charset designation: ESC ( final.
		if len(data) < 3 {
			return 0, escOther, false
		}
		return 3, escOther, true
	default:
		// Two-byte sequence (ESC 7, ESC 8, ESC =, ...).
		return 2, escOther, true
	}
}

// parseCSI classifies a CSI sequence; data starts with ESC [.
func parseCSI(data []byte) (n int, kind escKind, complete bool) {
	i := 2
	for i < len(data) && data[i] >= 0x20 && data[i] <= 0x3f {
		i++
	}
	if i >= len(data) {
		return 0, escOther, false
	}
	final := data[i]
	if final < 0x40 || final > 0x7e {
		// Malformed; consume the ESC alone and move on.
		return 1, escOther, true
	}
	body := string(data[2:i])
	n = i + 1

	switch final {
	case 'r':
		if body == "" {
			return n, escRegionReset, true
		}
	case 'J':
		// ED. Parameter 1 (erase above the cursor) cannot reach the bottom
		// row; everything else can, including the selective-erase ?-variants.
		if body != "1" && body != "?1" {
			return n, escClear, true
		}
	case 'p':
		if body == "!" {
			return n, escReset, true // DECSTR resets the margins
		}
	case 'h', 'l':
		if len(body) > 0 && body[0] == '?' {
			for _, param := range splitParams(body[1:]) {
				if param == "47" || param == "1047" || param == "1049" {
					return n, escAltScreen, true
				}
			}
		}
	}
	return n, escOther, true
}

// splitParams splits a CSI parameter string on ';' without allocating a
// regexp's worth of machinery.
func splitParams(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ';' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}
