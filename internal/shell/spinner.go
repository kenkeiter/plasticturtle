package shell

import (
	"fmt"
	"io"
	"strings"
)

// spinnerFrames is the animation. Braille dots rather than ASCII because the
// line is redrawn in place and every frame must be the same display width.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinner is the boot-wait progress indicator.
//
// It animates only on a terminal. Without one — a script, a CI run, output
// being captured — the frames would be thousands of carriage returns in a log
// file, so the label is printed once instead and nothing is redrawn.
type spinner struct {
	w     io.Writer
	label string
	tty   bool
	frame int
	drawn bool
}

// spinner returns an indicator that has not announced itself yet.
//
// The first frame is deliberately left to the caller's first tick, which comes
// only after a poll has found the instance not yet ready. Announcing in the
// constructor would make every attach to an already-running VM print "waiting
// for VM to boot…" before immediately succeeding — telling the user the tool
// did something slow when it did nothing at all. The cost is that a genuine
// boot stays silent for one poll interval.
func (r *runner) spinner(label string) *spinner {
	return &spinner{w: r.msg, label: label, tty: r.o.TTY != nil}
}

// tick draws the next frame. On a terminal it rewrites the same line; anywhere
// else only the first call prints anything.
func (s *spinner) tick() {
	if s == nil || s.w == nil {
		return
	}
	if !s.tty {
		if !s.drawn {
			s.drawn = true
			fmt.Fprintf(s.w, "%s…\n", s.label)
		}
		return
	}
	frame := spinnerFrames[s.frame%len(spinnerFrames)]
	s.frame++
	s.drawn = true
	fmt.Fprintf(s.w, "\r%s %s… ", frame, s.label)
}

// stop erases the animated line so that whatever is printed next — an error,
// the guest's own first output — starts on a clean row.
func (s *spinner) stop() {
	if s == nil || s.w == nil || !s.tty || !s.drawn {
		return
	}
	// +4 covers the frame, the two spaces and the ellipsis.
	fmt.Fprintf(s.w, "\r%s\r", strings.Repeat(" ", len([]rune(s.label))+4))
	s.drawn = false
}
