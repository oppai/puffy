package tui

import "strings"

// Braille is a 2x4-dots-per-character drawing surface. It gives the terminal
// line chart eight times the resolution of a block sparkline, which is what
// makes a latency curve readable at one character per second of history.
type Braille struct {
	cols, rows int
	cells      []rune // rows*cols, dot bitmaps
}

// Dot bit layout inside one braille cell, as defined by Unicode block U+2800:
// the left column is bits 0,1,2,6 top to bottom and the right column is bits
// 3,4,5,7. The fourth row's bits being out of sequence is why this is a table
// rather than a shift.
var dotBits = [4][2]rune{
	{0x01, 0x08},
	{0x02, 0x10},
	{0x04, 0x20},
	{0x40, 0x80},
}

// NewBraille allocates a canvas cols characters wide and rows characters tall,
// addressable as cols*2 by rows*4 dots.
func NewBraille(cols, rows int) *Braille {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	return &Braille{cols: cols, rows: rows, cells: make([]rune, cols*rows)}
}

// DotW and DotH are the canvas size in dots.
func (b *Braille) DotW() int { return b.cols * 2 }
func (b *Braille) DotH() int { return b.rows * 4 }

// Set lights the dot at (x, y), with y measured downwards from the top.
// Out-of-range coordinates are dropped rather than clamped: clamping would draw
// a fake value at the edge of the plot.
func (b *Braille) Set(x, y int) {
	if x < 0 || y < 0 || x >= b.DotW() || y >= b.DotH() {
		return
	}
	cx, cy := x/2, y/4
	b.cells[cy*b.cols+cx] |= dotBits[y%4][x%2]
}

// VLine lights every dot in column x between y0 and y1 inclusive.
func (b *Braille) VLine(x, y0, y1 int) {
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	for y := y0; y <= y1; y++ {
		b.Set(x, y)
	}
}

// Line draws a straight segment with Bresenham, so a rising curve reads as
// connected instead of as a dotted stair.
func (b *Braille) Line(x0, y0, x1, y1 int) {
	dx, dy := abs(x1-x0), abs(y1-y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx - dy
	for {
		b.Set(x0, y0)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
}

// Rows renders the canvas, one string per character row. Empty cells become
// spaces rather than U+2800 so that trailing whitespace collapses and the
// surface shows through.
func (b *Braille) Rows() []string {
	out := make([]string, b.rows)
	var sb strings.Builder
	for y := range b.rows {
		sb.Reset()
		for x := range b.cols {
			if c := b.cells[y*b.cols+x]; c == 0 {
				sb.WriteByte(' ')
			} else {
				sb.WriteRune(0x2800 + c)
			}
		}
		out[y] = strings.TrimRight(sb.String(), " ")
	}
	return out
}
