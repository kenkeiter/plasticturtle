// Package banner prints the plasticturtle logo.
//
// The art in art.go is generated from doc/logo-lg.png; see doc/logo-render.go
// for how to regenerate it.
package banner

import (
	"io"
	"os"

	"golang.org/x/term"
)

// Fprint writes the logo to w, followed by a blank line, when w is a terminal
// that can show it. Otherwise it writes nothing at all.
//
// Nothing is the right fallback because the logo is made entirely of color:
// every cell is a half-block whose shape carries no meaning without its
// foreground and background, so a terminal that drops the escapes renders 23
// lines of noise rather than a degraded turtle. Writing nothing also keeps the
// escapes out of pipes and files, where they would corrupt output that a
// script is reading.
func Fprint(w io.Writer) {
	f, ok := w.(*os.File)
	if !ok || !fits(f) {
		return
	}
	io.WriteString(w, Art+"\n")
}

// fits reports whether f is a terminal the banner can be drawn on.
func fits(f *os.File) bool {
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return false
	}
	cols, rows, err := term.GetSize(fd)
	if err != nil {
		return false
	}
	return canRender(cols, rows, os.Getenv("NO_COLOR"), os.Getenv("TERM"))
}

// canRender holds the policy on its own so it can be tested without a terminal.
//
// The window must fit the art in both directions: too narrow and every line
// wraps, which shears the turtle in half, and too short and the logo cannot be
// seen whole in the first place. Fitting the art is all this checks — what
// follows the banner can still push it off the top of a short window.
func canRender(cols, rows int, noColor, termEnv string) bool {
	if noColor != "" {
		return false
	}
	if termEnv == "" || termEnv == "dumb" {
		return false
	}
	return cols >= Cols && rows >= Rows
}
