// Command logo-render draws a PNG in the terminal using half-block characters,
// packing two image rows into each terminal line so pixels stay square.
//
// It's a prototype for the `pt` banner:
//
//	go run doc/logo-render.go
//	go run doc/logo-render.go -w 32 -mode 256
//
// The banner the CLI prints is regenerated with:
//
//	go run doc/logo-render.go -w 66 -go > internal/banner/art.go
//
// Fully transparent pixels are left as the terminal's own background, so the
// banner sits on light and dark themes alike.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/format"
	"image"
	_ "image/png"
	"math"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

const (
	upperHalf = "▀"
	lowerHalf = "▄"

	// Coverage at or above which an averaged cell counts as opaque.
	alphaCutoff = 0.5
)

func main() {
	var (
		path   = flag.String("f", "doc/logo-lg.png", "PNG to render")
		width  = flag.Int("w", 60, "target width in terminal columns")
		mode   = flag.String("mode", "truecolor", "color mode: truecolor or 256")
		filter = flag.String("filter", "nearest", "downsampling: nearest (pixel art) or box (photos)")
		pad    = flag.Int("pad", 0, "left padding in columns")
		asGo   = flag.Bool("go", false, "emit a Go source file holding the rendered art")
	)
	flag.Parse()

	if err := run(*path, *width, *mode, *filter, *pad, *asGo); err != nil {
		fmt.Fprintln(os.Stderr, "logo-render:", err)
		os.Exit(1)
	}
}

func run(path string, width int, mode, filter string, pad int, asGo bool) error {
	var paint painter
	switch mode {
	case "truecolor":
		paint = truecolorPainter{}
	case "256":
		paint = xterm256Painter{}
	default:
		return fmt.Errorf("unknown color mode %q", mode)
	}
	if filter != "nearest" && filter != "box" {
		return fmt.Errorf("unknown filter %q", filter)
	}

	// Never overflow the window we're actually drawing into.
	if !asGo && term.IsTerminal(int(os.Stdout.Fd())) {
		if cols, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && cols > 0 {
			width = min(width, cols-pad)
		}
	}
	if width < 1 {
		return fmt.Errorf("width must be at least 1 column")
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	src, _, err := image.Decode(f)
	if err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}

	box := opaqueBounds(src)
	if box.Empty() {
		return fmt.Errorf("%s is entirely transparent", path)
	}

	// One column per cell, two half-rows per cell: sample on a square grid so
	// the art keeps its aspect ratio.
	cell := float64(box.Dx()) / float64(width)
	rows := int(math.Round(float64(box.Dy()) / cell))
	grid := sample(src, box, width, rows, cell, filter == "nearest")

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	if asGo {
		return emitGo(out, grid, width, rows, paint, pad)
	}
	draw(out, grid, width, rows, paint, pad)
	return nil
}

// swatch is one sampled half-pixel: a color plus how much of it was covered.
type swatch struct {
	r, g, b float64 // sRGB, 0..1
	solid   bool
}

// opaqueBounds is the tightest rectangle containing every pixel solid enough to
// survive rendering, so surrounding empty space doesn't eat into the budgeted
// columns. The cutoff matters: this logo carries a faint alpha halo out to the
// image edges, and cropping on a > 0 would keep the whole canvas.
func opaqueBounds(src image.Image) image.Rectangle {
	b := src.Bounds()
	box := image.Rectangle{Min: b.Max, Max: b.Min}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := src.At(x, y).RGBA(); float64(a)/0xffff >= alphaCutoff {
				box.Min.X = min(box.Min.X, x)
				box.Min.Y = min(box.Min.Y, y)
				box.Max.X = max(box.Max.X, x+1)
				box.Max.Y = max(box.Max.Y, y+1)
			}
		}
	}
	if box.Min.X >= box.Max.X || box.Min.Y >= box.Max.Y {
		return image.Rectangle{}
	}
	return box
}

// sample reduces src to a cols x rows grid of half-pixels. Each cell covers one
// source rectangle, which is either averaged (box) or read at its center pixel
// (nearest). Averaging happens in linear light and is weighted by alpha, which
// keeps antialiased edges from darkening toward the transparent void.
func sample(src image.Image, box image.Rectangle, cols, rows int, cell float64, nearest bool) []swatch {
	grid := make([]swatch, cols*rows)
	for row := range rows {
		ry0 := box.Min.Y + int(math.Round(float64(row)*cell))
		ry1 := min(max(box.Min.Y+int(math.Round(float64(row+1)*cell)), ry0+1), box.Max.Y)

		for col := range cols {
			rx0 := box.Min.X + int(math.Round(float64(col)*cell))
			rx1 := min(max(box.Min.X+int(math.Round(float64(col+1)*cell)), rx0+1), box.Max.X)

			x0, y0, x1, y1 := rx0, ry0, rx1, ry1
			// Pixel art lands close to 1:1 here, where box-averaging beats
			// against the source grid and smears fine detail — the turtle
			// loses its mouth. Reading the center pixel keeps edges hard.
			if nearest {
				x0, y0 = (rx0+rx1)/2, (ry0+ry1)/2
				x1, y1 = x0+1, y0+1
			}

			var lr, lg, lb, weight float64
			var count int
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, b, a := src.At(x, y).RGBA()
					count++
					if a == 0 {
						continue
					}
					// RGBA() is alpha-premultiplied; undo that before
					// linearizing, then re-weight by alpha.
					af := float64(a) / 0xffff
					lr += af * toLinear(float64(r)/float64(a))
					lg += af * toLinear(float64(g)/float64(a))
					lb += af * toLinear(float64(b)/float64(a))
					weight += af
				}
			}

			s := swatch{solid: count > 0 && weight/float64(count) >= alphaCutoff}
			if s.solid {
				s.r = toSRGB(lr / weight)
				s.g = toSRGB(lg / weight)
				s.b = toSRGB(lb / weight)
			}
			grid[row*cols+col] = s
		}
	}
	return grid
}

func draw(out *bufio.Writer, grid []swatch, cols, rows int, paint painter, pad int) {
	for _, line := range lines(grid, cols, rows, paint, pad) {
		out.WriteString(line)
		out.WriteByte('\n')
	}
}

// lines renders the grid two half-rows at a time: the upper half-block's
// foreground is the top pixel and its background is the bottom one. Color
// escapes are only emitted where the color actually changes, which cuts the
// output to roughly a third of the naive per-cell reset.
func lines(grid []swatch, cols, rows int, paint painter, pad int) []string {
	var out []string
	prefix := strings.Repeat(" ", pad)

	for row := 0; row < rows; row += 2 {
		at := func(col int) (top, bottom swatch) {
			top = grid[row*cols+col]
			if row+1 < rows {
				bottom = grid[(row+1)*cols+col]
			}
			return top, bottom
		}

		// Stop at the last cell with ink so trailing blanks cost nothing.
		last := -1
		for col := range cols {
			if top, bottom := at(col); top.solid || bottom.solid {
				last = col
			}
		}
		if last < 0 {
			out = append(out, "")
			continue
		}

		var line strings.Builder
		line.WriteString(prefix)
		var curFg, curBg string

		for col := 0; col <= last; col++ {
			top, bottom := at(col)

			// A half-block paints its foreground in the half the glyph fills
			// and lets the background show through the other half, so a cell
			// with one transparent half picks the glyph that leaves that half
			// to the terminal's own background.
			var wantFg, wantBg, glyph string
			switch {
			case !top.solid && !bottom.solid:
				glyph = " "
			case top.solid && !bottom.solid:
				wantFg, glyph = paint.fg(top), upperHalf
			case !top.solid && bottom.solid:
				wantFg, glyph = paint.fg(bottom), lowerHalf
			default:
				wantFg, wantBg, glyph = paint.fg(top), paint.bg(bottom), upperHalf
			}

			if wantBg != curBg {
				if wantBg == "" {
					line.WriteString(defaultBg)
				} else {
					line.WriteString(wantBg)
				}
				curBg = wantBg
			}
			if wantFg != "" && wantFg != curFg {
				line.WriteString(wantFg)
				curFg = wantFg
			}
			line.WriteString(glyph)
		}

		line.WriteString(reset)
		out = append(out, line.String())
	}
	return out
}

const (
	reset     = "\x1b[0m"
	defaultBg = "\x1b[49m"
)

// displayWidth is how many terminal cells a rendered line occupies. Every
// escape lines() writes is an SGR sequence — CSI, digits and semicolons, then
// 'm' — and none of them advance the cursor.
func displayWidth(line string) int {
	cells := 0
	for i := 0; i < len(line); {
		if strings.HasPrefix(line[i:], "\x1b[") {
			if end := strings.IndexByte(line[i:], 'm'); end >= 0 {
				i += end + 1
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(line[i:])
		i += size
		cells++
	}
	return cells
}

// painter turns a swatch into the escape sequence that sets it as a foreground
// or background color.
type painter interface {
	fg(swatch) string
	bg(swatch) string
}

type truecolorPainter struct{}

func (truecolorPainter) fg(s swatch) string {
	r, g, b := s.bytes()
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
}

func (truecolorPainter) bg(s swatch) string {
	r, g, b := s.bytes()
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", r, g, b)
}

type xterm256Painter struct{}

func (xterm256Painter) fg(s swatch) string { return fmt.Sprintf("\x1b[38;5;%dm", xterm256(s)) }
func (xterm256Painter) bg(s swatch) string { return fmt.Sprintf("\x1b[48;5;%dm", xterm256(s)) }

func (s swatch) bytes() (int, int, int) {
	q := func(v float64) int {
		return int(math.Round(math.Min(math.Max(v, 0), 1) * 255))
	}
	return q(s.r), q(s.g), q(s.b)
}

// xterm256 picks the nearer of the 6x6x6 color cube and the 24-step gray ramp.
func xterm256(s swatch) int {
	r, g, b := s.bytes()

	cubeIdx := func(v int) int {
		return int(math.Round(math.Max(float64(v)-55, 0) / 40))
	}
	cubeVal := func(i int) int {
		if i == 0 {
			return 0
		}
		return 55 + 40*i
	}
	ri, gi, bi := cubeIdx(r), cubeIdx(g), cubeIdx(b)
	cube := 16 + 36*ri + 6*gi + bi
	cubeErr := dist(r, g, b, cubeVal(ri), cubeVal(gi), cubeVal(bi))

	gi2 := int(math.Round((float64(r+g+b)/3 - 8) / 10))
	gi2 = min(max(gi2, 0), 23)
	gv := 8 + 10*gi2
	if dist(r, g, b, gv, gv, gv) < cubeErr {
		return 232 + gi2
	}
	return cube
}

func dist(r1, g1, b1, r2, g2, b2 int) int {
	dr, dg, db := r1-r2, g1-g2, b1-b2
	return dr*dr + dg*dg + db*db
}

// toLinear and toSRGB convert between sRGB and linear light so that averaging
// several source pixels into one cell doesn't shift its brightness.
func toLinear(c float64) float64 {
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func toSRGB(c float64) float64 {
	if c <= 0.0031308 {
		return c * 12.92
	}
	return 1.055*math.Pow(c, 1/2.4) - 0.055
}

// emitGo writes the rendered art as a Go source file, so the CLI can print the
// banner without carrying a PNG decoder.
//
// The art's own dimensions ship with it: a caller that has to decide whether
// the banner fits the window would otherwise have to re-derive them by parsing
// the escapes back out.
func emitGo(out *bufio.Writer, grid []swatch, cols, rows int, paint painter, pad int) error {
	drawn := lines(grid, cols, rows, paint, pad)

	widest := 0
	for _, line := range drawn {
		widest = max(widest, displayWidth(line))
	}

	var src strings.Builder
	src.WriteString("// Code generated by doc/logo-render.go. DO NOT EDIT.\n\n")
	src.WriteString("package banner\n\n")
	src.WriteString("// Cols is the widest line of Art measured in terminal cells, and Rows the\n")
	src.WriteString("// number of lines it occupies. Neither can be read off Art directly, whose\n")
	src.WriteString("// length is dominated by color escapes that take up no space on screen.\n")
	fmt.Fprintf(&src, "const (\n\tCols = %d\n\tRows = %d\n)\n\n", widest, len(drawn))
	src.WriteString("// Art is the plasticturtle logo as ANSI-colored half-blocks.\n")
	src.WriteString("const Art = \"\" +\n")
	for _, line := range drawn {
		fmt.Fprintf(&src, "\t%q +\n", line+"\n")
	}
	src.WriteString("\t\"\"\n")

	formatted, err := format.Source([]byte(src.String()))
	if err != nil {
		return fmt.Errorf("format generated source: %w", err)
	}
	_, err = out.Write(formatted)
	return err
}
