package tui

import (
	"io"
	"os"
	"strings"
	"testing"
)

// ttyScreen returns a Screen that believes it is attached to a terminal, plus a
// reader for what it wrote. A real pty is not available in every test
// environment, and the escape sequences are the thing under test, not the
// kernel's terminal handling.
func ttyScreen(t *testing.T, cols, rows int) (*Screen, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })

	s := NewScreen(w, "dark", true, false)
	s.tty = true
	s.cols, s.rows = cols, rows
	return s, r
}

// read drains whatever is buffered without blocking on the empty pipe.
func read(t *testing.T, r *os.File, w io.Closer) string {
	t.Helper()
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The live view must not scroll the user's scrollback away, and must put the
// terminal back the way it found it.
func TestAltScreenIsEnteredAndRestored(t *testing.T) {
	s, r := ttyScreen(t, 80, 24)
	s.EnterAlt()
	s.LeaveAlt()
	got := read(t, r, s.out.(*os.File))

	for _, want := range []string{
		"\x1b[?1049h", // enter alternate screen
		"\x1b[?25l",   // hide cursor
		"\x1b[?25h",   // show cursor again
		"\x1b[?1049l", // leave alternate screen
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Index(got, "\x1b[?1049h") > strings.Index(got, "\x1b[?1049l") {
		t.Error("left the alternate screen before entering it")
	}
}

// EnterAlt must be inert when the destination is not a terminal, or piping puffy
// into a file would fill it with escape sequences.
func TestAltScreenSkippedWhenNotATTY(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	s := NewScreen(w, "dark", false, false)
	s.EnterAlt()
	s.LeaveAlt()
	if got := read(t, r, w); got != "" {
		t.Errorf("wrote %q to a non-terminal", got)
	}
}

// Frames repaint in place: home the cursor, then clear each line as it is
// written. A clear-screen between frames reads as a blink at one frame a second.
func TestFrameRepaintsWithoutClearingTheScreen(t *testing.T) {
	s, r := ttyScreen(t, 80, 24)
	s.Frame([]string{"alpha", "beta"})
	got := read(t, r, s.out.(*os.File))

	if !strings.HasPrefix(got, "\x1b[H") {
		t.Errorf("frame does not start by homing the cursor: %q", got)
	}
	if strings.Contains(got, "\x1b[2J") {
		t.Error("frame clears the whole screen, which flickers")
	}
	if n := strings.Count(got, "\x1b[K"); n != 2 {
		t.Errorf("erase-to-end-of-line count = %d, want one per line", n)
	}
}

// A frame shorter than the last must erase the rows the previous one left
// behind, or a shrinking hop table leaves stale rows on screen.
func TestFrameErasesTheTailOfATallerPreviousFrame(t *testing.T) {
	s, r := ttyScreen(t, 80, 24)
	s.Frame([]string{"a", "b", "c", "d"})
	s.Frame([]string{"a"})
	got := read(t, r, s.out.(*os.File))

	frames := strings.SplitN(got, "\x1b[H", 3)
	if len(frames) != 3 {
		t.Fatalf("expected two frames, got %d segments", len(frames)-1)
	}
	// The second frame writes one line plus three bare erases for the tail.
	if n := strings.Count(frames[2], "\x1b[K"); n != 4 {
		t.Errorf("second frame issued %d erases, want 4 (1 line + 3 stale rows)", n)
	}
}

func TestClampCountsPrintableColumnsOnly(t *testing.T) {
	c := Color{mode: modeTrue, dark: true}
	plain := "abcdefghij"
	if got := Clamp(plain, 4); got != "abcd" {
		t.Errorf("Clamp(plain, 4) = %q, want %q", got, "abcd")
	}
	if got := Clamp(plain, 99); got != plain {
		t.Errorf("Clamp widened the string: %q", got)
	}
	if got := Clamp(plain, 0); got != "" {
		t.Errorf("Clamp(_, 0) = %q, want empty", got)
	}

	// An escape sequence costs no columns, so eight printable characters
	// survive a limit of eight however they are coloured.
	coloured := c.Paint(RoleSeries, "abcd") + c.Paint(RoleCritical, "efgh")
	got := Clamp(coloured, 8)
	if stripped := strip(got); stripped != "abcdefgh" {
		t.Errorf("Clamp kept %q printable, want %q", stripped, "abcdefgh")
	}

	// Cutting mid-colour must close it, or the colour bleeds into the rest of
	// the frame.
	cut := Clamp(c.FG(RoleCritical)+"abcdef", 3)
	if strip(cut) != "abc" {
		t.Errorf("cut %q printable, want %q", strip(cut), "abc")
	}
	if !strings.HasSuffix(cut, "\x1b[0m") {
		t.Errorf("cut inside a colour run without resetting it: %q", cut)
	}
}

// Multi-byte glyphs must never be split: half a braille character is a mojibake
// byte, not a narrower chart.
func TestClampNeverSplitsAGlyph(t *testing.T) {
	s := "▁▂▃▄▅" // three bytes each
	for w := 0; w <= 6; w++ {
		got := Clamp(s, w)
		if !isValidUTF8(got) {
			t.Fatalf("Clamp(%q, %d) = % x, which is not valid UTF-8", s, w, got)
		}
		want := min(w, 5)
		if n := len([]rune(got)); n != want {
			t.Errorf("Clamp(_, %d) kept %d glyphs, want %d", w, n, want)
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestEnvSizeOverridesDefault(t *testing.T) {
	t.Setenv("COLUMNS", "137")
	t.Setenv("LINES", "42")
	r, w, _ := os.Pipe()
	defer func() { r.Close(); w.Close() }()

	s := NewScreen(w, "dark", false, false)
	if cols, rows := s.Size(); cols != 137 || rows != 42 {
		t.Errorf("size = %dx%d, want 137x42 from the environment", cols, rows)
	}

	t.Setenv("COLUMNS", "0") // invalid values are ignored, not applied
	s2 := NewScreen(w, "dark", false, false)
	if cols, _ := s2.Size(); cols != 80 {
		t.Errorf("cols = %d, want the 80-column default for an invalid COLUMNS", cols)
	}
}

func TestThemeSelection(t *testing.T) {
	r, w, _ := os.Pipe()
	defer func() { r.Close(); w.Close() }()

	if c := NewScreen(w, "light", true, false).Color(); c.dark {
		t.Error("--theme light produced the dark palette")
	}
	if c := NewScreen(w, "dark", true, false).Color(); !c.dark {
		t.Error("--theme dark produced the light palette")
	}
	// A high COLORFGBG background index means the terminal is light.
	t.Setenv("COLORFGBG", "0;15")
	if c := NewScreen(w, "auto", true, false).Color(); c.dark {
		t.Error("auto theme ignored COLORFGBG reporting a light background")
	}
}

func TestNoColorEnvIsHonoured(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	r, w, _ := os.Pipe()
	defer func() { r.Close(); w.Close() }()

	c := NewScreen(w, "dark", true, false).Color()
	if got := c.Paint(RoleCritical, "x"); got != "x" {
		t.Errorf("NO_COLOR still coloured the output: %q", got)
	}
}

// The 256-colour fallback must land on a sane index for each palette role, and
// must not send near-neutral colours into the colour cube or hues into the
// greyscale ramp.
func TestXterm256Mapping(t *testing.T) {
	cases := map[string]struct{ lo, hi int }{
		"#000000": {16, 16},
		"#ffffff": {231, 231},
		"#898781": {232, 255}, // near-neutral: greyscale ramp
		"#2a78d6": {16, 231},  // a hue: colour cube
		"#d03b3b": {16, 231},
	}
	for hex, want := range cases {
		got := xterm256(hex)
		if got < want.lo || got > want.hi {
			t.Errorf("xterm256(%s) = %d, want within %d..%d", hex, got, want.lo, want.hi)
		}
	}
}

// Every role must resolve in both modes; a missing entry would silently render
// as the muted fallback and quietly break an encoding.
func TestEveryRoleHasBothModes(t *testing.T) {
	roles := append([]Role{
		RoleText, RoleSecondary, RoleMuted, RoleAxis, RoleSeries,
		RoleGood, RoleWarning, RoleSerious, RoleCritical,
	}, append(LatRamp, LossRamp...)...)

	for _, r := range roles {
		if _, ok := lightHex[r]; !ok {
			t.Errorf("role %d has no light value", r)
		}
		if _, ok := darkHex[r]; !ok {
			t.Errorf("role %d has no dark value", r)
		}
	}
}

// A sequential ramp has to let "near zero" recede into the surface. On a light
// surface that means climbing towards darker; on a dark surface, towards
// lighter. Getting this backwards inverts the encoding, so it is asserted
// rather than trusted to a comment.
func TestRampsRunTowardsTheSurfaceAtTheLowEnd(t *testing.T) {
	lum := func(hex string) float64 {
		r, g, b := hexRGB(hex)
		return 0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)
	}
	for _, ramp := range [][]Role{LatRamp, LossRamp} {
		// Light surface: magnitude climbs towards darker.
		for i := 1; i < len(ramp); i++ {
			if lum(lightHex[ramp[i]]) >= lum(lightHex[ramp[i-1]]) {
				t.Errorf("light ramp step %d is not darker than step %d", i, i-1)
			}
		}
		// Dark surface: magnitude climbs towards lighter.
		for i := 1; i < len(ramp); i++ {
			if lum(darkHex[ramp[i]]) <= lum(darkHex[ramp[i-1]]) {
				t.Errorf("dark ramp step %d is not lighter than step %d", i, i-1)
			}
		}
	}
}
