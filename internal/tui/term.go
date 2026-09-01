// Package tui renders puffy's live time-series graphs to a terminal.
package tui

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// Screen is a flicker-free full-redraw terminal surface. Each frame is written
// as "home, then overwrite every line, then erase what the previous frame left
// below" - never a clear-screen, which shows as a blink at one frame per second.
type Screen struct {
	out      io.Writer
	tty      bool
	color    Color
	rows     int
	cols     int
	prevRows int
	buf      strings.Builder
	alt      bool
}

// NewScreen inspects the output stream and returns a Screen configured for it.
// A non-terminal destination disables cursor control and colour so that piping
// puffy into a file or a pager produces clean text.
func NewScreen(out *os.File, theme string, forceColor, noColor bool) *Screen {
	s := &Screen{out: out, rows: 24, cols: 80}
	fd := int(out.Fd())
	s.tty = term.IsTerminal(fd)
	if s.tty {
		if c, r, err := term.GetSize(fd); err == nil {
			s.cols, s.rows = c, r
		}
	}
	// COLUMNS and LINES win where the ioctl cannot help: piping into a file or a
	// pager, capturing output for documentation, or a CI job with no terminal.
	if c := envInt("COLUMNS"); c > 0 {
		s.cols = c
	}
	if r := envInt("LINES"); r > 0 {
		s.rows = r
	}
	s.color = detectColor(theme, s.tty, forceColor, noColor)
	return s
}

func envInt(name string) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n < 1 || n > 10000 {
		return 0
	}
	return n
}

// IsTTY reports whether the destination is an interactive terminal.
func (s *Screen) IsTTY() bool { return s.tty }

// Color exposes the resolved palette.
func (s *Screen) Color() Color { return s.color }

// Size re-reads the terminal dimensions. Called once per frame so a window
// resize is picked up without a SIGWINCH handler. An explicit COLUMNS or LINES
// keeps its value: it was set deliberately, and a resize should not undo it.
func (s *Screen) Size() (cols, rows int) {
	if s.tty {
		if c, r, err := term.GetSize(int(s.out.(*os.File).Fd())); err == nil && c > 0 && r > 0 {
			if envInt("COLUMNS") == 0 {
				s.cols = c
			}
			if envInt("LINES") == 0 {
				s.rows = r
			}
		}
	}
	return s.cols, s.rows
}

// EnterAlt switches to the alternate screen buffer and hides the cursor, so the
// live view does not scroll the user's scrollback away.
func (s *Screen) EnterAlt() {
	if !s.tty || s.alt {
		return
	}
	s.alt = true
	fmt.Fprint(s.out, "\x1b[?1049h\x1b[?25l")
}

// LeaveAlt restores the original screen.
func (s *Screen) LeaveAlt() {
	if !s.alt {
		return
	}
	s.alt = false
	fmt.Fprint(s.out, "\x1b[?25h\x1b[?1049l")
}

// RestoreTerminal undoes EnterAlt by writing the sequences directly, touching
// none of the screen's own state. That makes it safe to call from a signal
// handler while a frame is being painted - which is exactly when it is needed,
// because the second Ctrl-C must not leave the shell on the alternate screen
// with no cursor.
func (s *Screen) RestoreTerminal() {
	if !s.tty {
		return
	}
	io.WriteString(s.out, "\x1b[?25h\x1b[?1049l")
}

// Frame paints the given lines. Lines longer than the terminal are left alone:
// the callers size their own content, and silently truncating a graph is worse
// than letting one row wrap while the window is mid-resize.
func (s *Screen) Frame(lines []string) {
	if !s.tty {
		for _, l := range lines {
			fmt.Fprintln(s.out, l)
		}
		return
	}
	s.buf.Reset()
	s.buf.WriteString("\x1b[H")
	for _, l := range lines {
		s.buf.WriteString(l)
		s.buf.WriteString("\x1b[K\r\n")
	}
	// Erase the tail of a previous, taller frame.
	for i := len(lines); i < s.prevRows; i++ {
		s.buf.WriteString("\x1b[K\r\n")
	}
	s.prevRows = len(lines)
	io.WriteString(s.out, s.buf.String())
}

// Print writes plain lines with no cursor control, for summaries after the live
// view has been torn down.
func (s *Screen) Print(lines []string) {
	for _, l := range lines {
		fmt.Fprintln(s.out, l)
	}
}

// Clamp truncates a possibly-coloured string to w printable columns. Escape
// sequences are copied through without being counted, and an active colour is
// closed with a reset, so cutting a line never bleeds its colour into the rest
// of the frame.
func Clamp(s string, w int) string {
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	printable, colored := 0, false
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			start := i
			for i < len(s) && s[i] != 'm' {
				i++
			}
			if i < len(s) {
				i++
			}
			esc := s[start:i]
			b.WriteString(esc)
			colored = esc != "\x1b[0m"
			continue
		}
		// Step over one whole rune so a multi-byte glyph is never cut in half.
		size := runeLen(s[i:])
		if printable+1 > w {
			if colored {
				b.WriteString("\x1b[0m")
			}
			return b.String()
		}
		b.WriteString(s[i : i+size])
		printable++
		i += size
	}
	return b.String()
}

// runeLen is the byte length of the leading UTF-8 rune. Only the length is
// needed: the renderers emit ASCII, box-drawing, braille and block glyphs, all
// of which occupy exactly one terminal column, so counting runes counts columns.
func runeLen(s string) int {
	if len(s) == 0 {
		return 0
	}
	switch b := s[0]; {
	case b < 0x80:
		return 1
	case b < 0xe0 && len(s) >= 2:
		return 2
	case b < 0xf0 && len(s) >= 3:
		return 3
	case len(s) >= 4:
		return 4
	default:
		return 1
	}
}

// --- colour -----------------------------------------------------------------

// mode is how colour is emitted.
type mode int

const (
	modeNone mode = iota
	mode256
	modeTrue
)

// Color resolves palette roles to escape sequences. Roles - not raw hex - are
// used throughout the renderers so light and dark swap in one place, and a
// no-colour terminal degrades to empty strings rather than to a special case in
// every call site.
type Color struct {
	mode mode
	dark bool
}

func detectColor(theme string, tty, force, none bool) Color {
	c := Color{dark: true}
	switch theme {
	case "light":
		c.dark = false
	case "dark":
		c.dark = true
	default:
		// COLORFGBG is "fg;bg"; a high background index means a light theme.
		if fgbg := os.Getenv("COLORFGBG"); fgbg != "" {
			if i := strings.LastIndex(fgbg, ";"); i >= 0 {
				if bg, err := strconv.Atoi(fgbg[i+1:]); err == nil && bg >= 7 {
					c.dark = false
				}
			}
		}
	}

	switch {
	case none || os.Getenv("NO_COLOR") != "":
		c.mode = modeNone
	case !tty && !force:
		c.mode = modeNone
	case os.Getenv("COLORTERM") == "truecolor" || os.Getenv("COLORTERM") == "24bit":
		c.mode = modeTrue
	case os.Getenv("TERM") == "dumb":
		c.mode = modeNone
	default:
		c.mode = mode256
	}
	return c
}

// Reset ends any active attribute run.
func (c Color) Reset() string {
	if c.mode == modeNone {
		return ""
	}
	return "\x1b[0m"
}

// FG returns the foreground sequence for a palette role.
func (c Color) FG(r Role) string {
	if c.mode == modeNone {
		return ""
	}
	h := c.hex(r)
	if c.mode == modeTrue {
		r8, g8, b8 := hexRGB(h)
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r8, g8, b8)
	}
	return fmt.Sprintf("\x1b[38;5;%dm", xterm256(h))
}

// Paint wraps s in the role's colour.
func (c Color) Paint(r Role, s string) string {
	if c.mode == modeNone {
		return s
	}
	return c.FG(r) + s + c.Reset()
}

// Dim wraps s in the muted ink role, for chrome that must recede.
func (c Color) Dim(s string) string { return c.Paint(RoleMuted, s) }

// Bold emphasises without using colour, so it survives NO_COLOR.
func (c Color) Bold(s string) string {
	if c.mode == modeNone {
		return s
	}
	return "\x1b[1m" + s + "\x1b[22m"
}

func hexRGB(h string) (int, int, int) {
	v, err := strconv.ParseUint(strings.TrimPrefix(h, "#"), 16, 32)
	if err != nil {
		return 200, 200, 200
	}
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)
}

// xterm256 maps a hex colour to the nearest xterm-256 index, preferring the
// 6x6x6 colour cube and falling back to the greyscale ramp when the colour is
// close to neutral.
func xterm256(h string) int {
	r, g, b := hexRGB(h)
	// Greyscale ramp (232-255) for near-neutral colours.
	if abs(r-g) < 12 && abs(g-b) < 12 && abs(r-b) < 12 {
		grey := (r + g + b) / 3
		if grey < 8 {
			return 16
		}
		if grey > 238 {
			return 231
		}
		return 232 + (grey-8)*24/230
	}
	q := func(v int) int {
		// Cube levels are 0,95,135,175,215,255; midpoints decide the bucket.
		switch {
		case v < 48:
			return 0
		case v < 115:
			return 1
		case v < 155:
			return 2
		case v < 195:
			return 3
		case v < 235:
			return 4
		default:
			return 5
		}
	}
	return 16 + 36*q(r) + 6*q(g) + q(b)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
