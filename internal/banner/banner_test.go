package banner

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFprintSkipsNonTerminals(t *testing.T) {
	var out bytes.Buffer
	Fprint(&out)
	if out.Len() != 0 {
		t.Errorf("wrote %d bytes to a non-terminal", out.Len())
	}
}

func TestCanRender(t *testing.T) {
	tests := []struct {
		name       string
		cols, rows int
		noColor    string
		term       string
		want       bool
	}{
		{"roomy window", 120, 40, "", "xterm-256color", true},
		{"exact fit", Cols, Rows, "", "xterm-256color", true},
		{"too narrow", Cols - 1, 40, "", "xterm-256color", false},
		{"too short", 120, Rows - 1, "", "xterm-256color", false},
		{"NO_COLOR set", 120, 40, "1", "xterm-256color", false},
		{"NO_COLOR empty means unset", 120, 40, "", "xterm-256color", true},
		{"dumb terminal", 120, 40, "", "dumb", false},
		{"no TERM", 120, 40, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canRender(tt.cols, tt.rows, tt.noColor, tt.term); got != tt.want {
				t.Errorf("canRender(%d, %d, %q, %q) = %v, want %v",
					tt.cols, tt.rows, tt.noColor, tt.term, got, tt.want)
			}
		})
	}
}

// TestArtMatchesDimensions guards the generated constants against art.go being
// regenerated at a different width without them being updated alongside.
func TestArtMatchesDimensions(t *testing.T) {
	lines := strings.Split(strings.TrimSuffix(Art, "\n"), "\n")
	if len(lines) != Rows {
		t.Errorf("Art has %d lines, Rows says %d", len(lines), Rows)
	}

	widest := 0
	for _, line := range lines {
		widest = max(widest, cells(line))
	}
	if widest != Cols {
		t.Errorf("widest line is %d cells, Cols says %d", widest, Cols)
	}
}

// cells counts the printable width of a line, skipping the SGR escapes.
func cells(line string) int {
	n := 0
	for i := 0; i < len(line); {
		if strings.HasPrefix(line[i:], "\x1b[") {
			if end := strings.IndexByte(line[i:], 'm'); end >= 0 {
				i += end + 1
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(line[i:])
		i += size
		n++
	}
	return n
}
