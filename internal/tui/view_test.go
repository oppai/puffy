package tui

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oppai/puffy/internal/model"
)

// synthetic builds a session with a known shape: a clean path whose middle hop
// stalls for a stretch and whose last-but-one hop drops a burst of probes. Both
// panels should therefore have something to show.
func synthetic(rounds int) *model.Session {
	s := &model.Session{
		Tool: "puffy", Version: "test", Mode: "trace",
		Target: "example.test", TargetIP: "192.0.2.9", TargetName: "example.test",
		StartedAt: time.Unix(0, 0), EndedAt: time.Unix(int64(rounds), 0),
		IntervalMS: 1000, TimeoutMS: 2000, Rounds: rounds,
	}
	base := []float64{1.4, 12, 24, 31, 44, 47}
	for i, b := range base {
		h := &model.Hop{TTL: i + 1, Addrs: []string{"198.51.100." + string(rune('1'+i))}}
		h.Final = i == len(base)-1
		for r := range rounds {
			sm := model.Sample{Round: r, OffsetMS: float64(r) * 1000, Addr: h.Addrs[0]}
			// A wave of jitter everywhere, so the chart is not a straight line.
			v := b + math.Sin(float64(r)/3)*b*0.08
			switch {
			case i == 2 && r >= rounds/3 && r < rounds/3+rounds/6:
				v = b * 5 // the stall
			case i == 4 && r >= rounds/2 && r < rounds/2+rounds/8:
				sm.RTTMS = nil // the loss burst
				h.Samples = append(h.Samples, sm)
				continue
			}
			val := v
			sm.RTTMS = &val
			h.Samples = append(h.Samples, sm)
		}
		s.Hops = append(s.Hops, h)
	}
	s.Analyze()
	return s
}

func testScreen(t *testing.T, cols, rows int, dark bool) *Screen {
	t.Helper()
	s := NewScreen(os.Stdout, "dark", true, false)
	if !dark {
		s.color.dark = false
	}
	// Override the probed terminal size so the layout is deterministic.
	s.tty = false
	s.cols, s.rows = cols, rows
	return s
}

// strip removes ANSI escapes so assertions and width checks see printable text.
func strip(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++ // the 'm'
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestTraceSummaryFitsWidthAndShowsBothPanels(t *testing.T) {
	s := synthetic(60)
	v := View{Screen: testScreen(t, 120, 40, true)}
	lines := v.Summary(s)

	joined := strip(strings.Join(lines, "\n"))
	for _, want := range []string{
		"ttl", "host", "loss", "worst", "rtt over time",
		"packet loss", "latency stalls", "what the graph shows",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("summary is missing %q\n%s", want, joined)
		}
	}

	for i, l := range lines {
		if w := len([]rune(strip(l))); w > 120 {
			t.Errorf("line %d is %d columns wide, over the 120 available:\n%s", i, w, strip(l))
		}
	}
}

// Every hop row must carry a time series. A row that silently loses its
// sparkline is the failure that would make the tool useless without erroring.
func TestTraceRowsAllHaveSparklines(t *testing.T) {
	s := synthetic(40)
	v := View{Screen: testScreen(t, 100, 40, true)}
	lines := v.Summary(s)

	found := 0
	for _, l := range lines {
		txt := strip(l)
		if !strings.HasPrefix(txt, "  ") {
			continue
		}
		if strings.ContainsAny(txt, "▁▂▃▄▅▆▇█×") {
			found++
		}
	}
	if found < len(s.Hops) {
		t.Errorf("only %d of %d hops rendered a series\n%s", found, len(s.Hops),
			strip(strings.Join(lines, "\n")))
	}
}

func TestNarrowTerminalStillRenders(t *testing.T) {
	s := synthetic(30)
	for _, cols := range []int{40, 60, 80, 200} {
		v := View{Screen: testScreen(t, cols, 30, true)}
		lines := v.Live(s)
		if len(lines) == 0 {
			t.Fatalf("cols=%d produced no output", cols)
		}
		for i, l := range lines {
			if w := len([]rune(strip(l))); w > cols {
				t.Errorf("cols=%d: line %d is %d wide:\n%s", cols, i, w, strip(l))
			}
		}
	}
}

func TestLiveTraceRespectsTerminalHeight(t *testing.T) {
	s := synthetic(30)
	for _, rows := range []int{10, 16, 24, 50} {
		v := View{Screen: testScreen(t, 120, rows, true)}
		if n := len(v.Live(s)); n > rows {
			t.Errorf("rows=%d: rendered %d lines, which would scroll the live view", rows, n)
		}
	}
}

func TestPingFrameRendersChartAndLossStrip(t *testing.T) {
	s := synthetic(60)
	s.Mode = "ping"
	s.Hops = s.Hops[len(s.Hops)-1:] // ping is a one-hop session
	s.Recompute()

	v := View{Screen: testScreen(t, 100, 30, true)}
	joined := strip(strings.Join(v.Live(s), "\n"))
	if !strings.Contains(joined, "loss") {
		t.Errorf("ping frame has no loss row:\n%s", joined)
	}
	if !strings.Contains(joined, "rtt ms") {
		t.Errorf("ping frame has no chart label:\n%s", joined)
	}
	// The braille plot must actually contain plotted dots.
	if !strings.ContainsFunc(joined, func(r rune) bool { return r >= 0x2801 && r <= 0x28ff }) {
		t.Errorf("ping frame drew no braille marks:\n%s", joined)
	}
}

// A hop that never answered must not be scored or drawn as if it had.
func TestAllTimeoutHopRendersAsUnknown(t *testing.T) {
	s := synthetic(20)
	dead := &model.Hop{TTL: 99}
	for r := range 20 {
		dead.Samples = append(dead.Samples, model.Sample{Round: r, OffsetMS: float64(r) * 1000})
	}
	s.Hops = append(s.Hops, dead)
	s.Recompute()

	v := View{Screen: testScreen(t, 120, 40, true)}
	var row string
	for _, l := range v.Summary(s) {
		if strings.Contains(strip(l), " 99 ") {
			row = strip(l)
			break
		}
	}
	if row == "" {
		t.Fatal("the unreachable hop was not rendered at all")
	}
	if !strings.Contains(row, "*") {
		t.Errorf("unreachable hop should be labelled *: %q", row)
	}
	if !strings.Contains(row, "100.0%") {
		t.Errorf("unreachable hop should read 100%% loss: %q", row)
	}
}

func TestNoColorProducesNoEscapes(t *testing.T) {
	s := synthetic(30)
	sc := NewScreen(os.Stdout, "dark", false, true)
	sc.tty = false
	sc.cols, sc.rows = 120, 40
	v := View{Screen: sc}
	for _, l := range v.Summary(s) {
		if strings.ContainsRune(l, 0x1b) {
			t.Fatalf("--no-color still emitted an escape sequence: %q", l)
		}
	}
}

// The heat glyphs must carry the magnitude order on their own, so the panel
// still reads with colour stripped, in greyscale print, or with any form of
// colour vision.
func TestHeatGlyphsAreOrderedWithoutColor(t *testing.T) {
	sc := NewScreen(os.Stdout, "dark", false, true)
	c := sc.Color()
	cells := []model.Cell{
		{Sent: 10, Lost: 0}, {Sent: 10, Lost: 1}, {Sent: 10, Lost: 3},
		{Sent: 10, Lost: 6}, {Sent: 10, Lost: 10},
	}
	for i := range cells {
		cells[i].LossPct = float64(cells[i].Lost) / float64(cells[i].Sent) * 100
	}
	got := HeatRow(c, cells, HeatLoss, 0)
	want := "·░▒▓█"
	if got != want {
		t.Errorf("heat row = %q, want %q (ordered glyphs, no colour)", got, want)
	}
}

func TestSparklineMarksTotalLossDistinctly(t *testing.T) {
	sc := NewScreen(os.Stdout, "dark", false, true)
	c := sc.Color()
	cells := []model.Cell{
		{Sent: 1, Lost: 0, MaxRTT: 10},
		{Sent: 1, Lost: 1, LossPct: 100},
		{Sent: 0},
		{Sent: 2, Lost: 1, LossPct: 50, MaxRTT: 80},
	}
	got := Sparkline(c, cells, 100, 10)
	if []rune(got)[1] != '×' {
		t.Errorf("a fully lost bucket should render ×, got %q", got)
	}
	if []rune(got)[2] != ' ' {
		t.Errorf("a bucket with no probes should render blank, got %q", got)
	}
}

func TestFmtSpanKeepsSubSecondPrecision(t *testing.T) {
	cases := map[float64]string{
		300: "300ms", 1000: "1s", 1500: "1.5s", 36000: "36s", 90000: "1m30s", 7200000: "2h0m",
	}
	for in, want := range cases {
		if got := fmtSpan(in); got != want {
			t.Errorf("fmtSpan(%v) = %q, want %q", in, got, want)
		}
	}
}

// Dumping the rendered frames is the only way to check the graphs actually look
// right; run with: go test ./internal/tui -run Eyeball -v
func TestEyeball(t *testing.T) {
	if os.Getenv("PUFFY_EYEBALL") == "" {
		t.Skip("set PUFFY_EYEBALL=1 to print the rendered frames")
	}
	s := synthetic(90)
	v := View{Screen: testScreen(t, 120, 44, true)}
	t.Log("\n" + strings.Join(v.Summary(s), "\n"))

	p := synthetic(90)
	p.Mode = "ping"
	p.Hops = p.Hops[len(p.Hops)-1:]
	p.Recompute()
	pv := View{Screen: testScreen(t, 110, 30, true)}
	t.Log("\n" + strings.Join(pv.Live(p), "\n"))
}

// percentFields are the loss columns of a hop row, in order: the window's first,
// then the session's.
func percentFields(row string) []string {
	var out []string
	for _, f := range strings.Fields(row) {
		if strings.HasSuffix(f, "%") {
			out = append(out, f)
		}
	}
	return out
}

// runeIndex is where sub starts in printable columns. The group rules are drawn
// with box characters, so a byte offset would not be a column.
func runeIndex(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return len([]rune(s[:i]))
}

// rowFor finds the table row for a ttl.
func rowFor(lines []string, ttl int) string {
	for _, l := range lines {
		txt := strip(l)
		if strings.HasPrefix(txt, fmt.Sprintf("%3d ", ttl)) {
			return txt
		}
	}
	return ""
}

// The live trace graph must show a fixed stretch of time however long the run
// has been going. Letting it cover "everything so far" is what made a long
// session's picture stop moving: every column kept absorbing more rounds.
func TestLiveTraceWindowDoesNotGrowWithTheSession(t *testing.T) {
	note := func(rounds int) string {
		v := View{Screen: testScreen(t, 120, 30, true)}
		for _, l := range v.Live(synthetic(rounds)) {
			if txt := strip(l); strings.Contains(txt, "of history,") {
				return strings.TrimSpace(txt)
			}
		}
		t.Fatalf("rounds=%d: the frame does not say what it is showing", rounds)
		return ""
	}
	short, long := note(120), note(60000)
	if short != long {
		t.Errorf("the graph changed shape as the run went on:\n  120 rounds: %s\n  60000 rounds: %s", short, long)
	}
	if !strings.HasSuffix(long, "1 round per column") {
		t.Errorf("the live window is %q; at a one-second interval every round should get a column of its own", long)
	}
}

// steady is a session whose replies repeat on a short cycle, so the sparkline is
// a pure function of which rounds are in view - and the vertical scale, which
// comes from the rounds drawn, is the same in every frame.
func steady(rounds int) *model.Session {
	s := &model.Session{
		Tool: "puffy", Mode: "trace", Target: "test", TargetIP: "192.0.2.1",
		StartedAt: time.Unix(0, 0), EndedAt: time.Unix(int64(rounds), 0),
		IntervalMS: 1000, TimeoutMS: 2000, Rounds: rounds,
	}
	h := &model.Hop{TTL: 1, Addrs: []string{"192.0.2.1"}, Final: true}
	// Only the tail is ever drawn, so only the tail needs to exist - which is
	// also what the collector hands the renderer on a long run.
	from := max(rounds-400, 0)
	for r := from; r < rounds; r++ {
		v := 20 + float64(r%7)*4
		h.Samples = append(h.Samples, model.Sample{
			Round: r, OffsetMS: float64(r) * 1000, RTTMS: &v, Addr: h.Addrs[0],
		})
	}
	s.SamplesFrom = from
	s.Hops = []*model.Hop{h}
	s.Recompute()
	return s
}

// sparkOf is the time series at the end of a hop row.
func sparkOf(row string) string {
	r := []rune(strings.TrimRight(row, " "))
	i := len(r)
	for i > 0 && strings.ContainsRune("▁▂▃▄▅▆▇█× ", r[i-1]) {
		i--
	}
	return strings.TrimSpace(string(r[i:]))
}

// The graph has to move. A window fixed in time quietly folds ten rounds into a
// column at a fast interval, and then the row only shifts every few seconds and
// mostly not even then, because a column shows the worst of its rounds - which
// is what "the graph stopped updating" looks like on a run that is drawing eight
// frames a second.
func TestLiveTraceGraphScrollsEveryRound(t *testing.T) {
	for _, interval := range []time.Duration{time.Second, 250 * time.Millisecond, 50 * time.Millisecond} {
		v := View{Screen: testScreen(t, 120, 30, true), Interval: interval}
		// A round of the graph is however many rounds one column holds.
		perCol := max(int(float64(liveColMS)/float64(interval.Milliseconds())), 1)
		before := sparkOf(rowFor(v.Live(steady(60000)), 1))
		after := sparkOf(rowFor(v.Live(steady(60000+perCol)), 1))
		if before == "" || after == "" {
			t.Fatalf("interval=%s: no series drawn", interval)
		}
		if before == after {
			t.Errorf("interval=%s: the series did not move after %d rounds:\n  %s", interval, perCol, before)
			continue
		}
		b, a := []rune(before), []rune(after)
		if len(a) != len(b) {
			t.Fatalf("interval=%s: the series changed width, %d then %d", interval, len(b), len(a))
		}
		if string(a[:len(a)-1]) != string(b[1:]) {
			t.Errorf("interval=%s: the series did not scroll by exactly one column\n  before %s\n  after  %s",
				interval, before, after)
		}
	}
}

// However fast the interval, one column may not swallow so much time that the
// picture stands still - nor so many probes that a frame reads the whole run.
func TestTraceWindowKeepsColumnsShortEnoughToMove(t *testing.T) {
	const sparkW = 30
	for _, c := range []struct {
		intervalMS float64
		wantPerCol int
	}{
		{5000, 1}, {1000, 1}, {250, 1}, {100, 3}, {50, 5}, {10, 25}, {0.5, maxLiveFold},
	} {
		window := traceWindow(sparkW, c.intervalMS, 0)
		if got := window / sparkW; got != c.wantPerCol {
			t.Errorf("at %vms a round, a column holds %d rounds, want %d", c.intervalMS, got, c.wantPerCol)
		}
		perCol := window / sparkW
		switch {
		case c.intervalMS >= liveColMS && perCol != 1:
			t.Errorf("at %vms a round, %d rounds share a column; a round that long deserves its own",
				c.intervalMS, perCol)
		case perCol > 1 && perCol < maxLiveFold && float64(perCol-1)*c.intervalMS >= liveColMS:
			t.Errorf("at %vms a round, a column holds %d rounds when %d would already be under the %dms cap",
				c.intervalMS, perCol, perCol-1, liveColMS)
		}
	}
	if got := traceWindow(sparkW, 1, 12345); got != 12345 {
		t.Error("an explicit --window must be obeyed as given")
	}
}

// The whole point of the split: a hop that lost probes an hour ago and is fine
// now must read as fine now and lossy over the run. One set of numbers cannot
// say both.
func TestHopRowSeparatesWindowFromSession(t *testing.T) {
	s := synthetic(400) // the loss burst is at rounds 200..250, long out of window
	v := View{Screen: testScreen(t, 120, 30, true)}
	row := rowFor(v.Live(s), 5)
	if row == "" {
		t.Fatal("the lossy hop has no row")
	}
	got := percentFields(row)
	if len(got) != 2 {
		t.Fatalf("row has %d loss columns, want the window's and the session's: %q", len(got), row)
	}
	if got[0] != "0.0%" {
		t.Errorf("window loss = %s, want 0.0%%: nothing was lost in the last minute (%q)", got[0], row)
	}
	if got[1] == "0.0%" {
		t.Errorf("session loss = %s, want the loss the run actually saw (%q)", got[1], row)
	}
}

// The group rules have to sit over the columns they describe, or they would
// mislabel the numbers they are there to explain.
func TestGroupHeaderLinesUpWithItsColumns(t *testing.T) {
	v := View{Screen: testScreen(t, 120, 30, true)}
	lines := v.Live(synthetic(400))
	var group, head string
	for i, l := range lines {
		if txt := strip(l); strings.Contains(txt, "window") && strings.Contains(txt, "session") {
			group, head = txt, strip(lines[i+1])
			break
		}
	}
	if group == "" {
		t.Fatalf("no group header:\n%s", strip(strings.Join(lines, "\n")))
	}
	if !strings.HasPrefix(strings.TrimSpace(head), "ttl host") {
		t.Fatalf("the row under the group header is not the column header: %q", head)
	}
	first := runeIndex(head, "loss")
	second := runeIndex(head[strings.Index(head, "loss")+1:], "loss") + first + 1
	// A column's value is right-aligned in seven columns, and the group rule
	// starts at the space before it.
	if got, want := runeIndex(group, "window"), first-3; got != want {
		t.Errorf("the window rule starts at column %d, but its first column is at %d\n%s\n%s", got, want, group, head)
	}
	if got, want := runeIndex(group, "session"), second-3; got != want {
		t.Errorf("the session rule starts at column %d, but its first column is at %d\n%s\n%s", got, want, group, head)
	}
}

// The summary is the long-term report, so its columns are the session's. Two
// identical groups side by side would only cost the graph its width.
func TestSummaryColumnsAreSessionScoped(t *testing.T) {
	v := View{Screen: testScreen(t, 120, 40, true)}
	lines := v.Summary(synthetic(400))
	row := rowFor(lines, 5)
	if got := percentFields(row); len(got) != 1 {
		t.Errorf("summary row has %d loss columns, want one: %q", len(got), row)
	}
	joined := strip(strings.Join(lines, "\n"))
	if !strings.Contains(joined, "session") {
		t.Errorf("the summary never says its numbers cover the session:\n%s", joined)
	}
}

// The heatmaps are drawn from the window, so their row list has to come from the
// window too - a hop that is quiet now would otherwise take up a row of blanks.
func TestPanelsDescribeTheWindowNotTheSession(t *testing.T) {
	v := View{Screen: testScreen(t, 120, 30, true)}
	joined := strip(strings.Join(v.Live(synthetic(400)), "\n"))
	if !strings.Contains(joined, "packet loss  none in the last ") {
		t.Errorf("a window with no loss should say so, and say over what:\n%s", joined)
	}
	// And with the trouble inside the window, the panel lists the hop.
	joined = strip(strings.Join(v.Live(synthetic(60)), "\n"))
	if !strings.Contains(joined, "packet loss —") {
		t.Errorf("loss inside the window is not listed:\n%s", joined)
	}
}

// steadyPair is two hops on the same short cycle, the second carrying one spike
// of spikeMS in the middle of the drawn window (0 for none).
func steadyPair(rounds int, spikeMS float64) *model.Session {
	s := steady(rounds)
	h := &model.Hop{TTL: 2, Addrs: []string{"192.0.2.2"}, Final: true}
	s.Hops[0].Final = false
	for i, sm := range s.Hops[0].Samples {
		v := *sm.RTTMS + 6
		if spikeMS > 0 && i == len(s.Hops[0].Samples)-8 {
			v = spikeMS
		}
		h.Samples = append(h.Samples, model.Sample{
			Round: sm.Round, OffsetMS: sm.OffsetMS, RTTMS: &v, Addr: h.Addrs[0],
		})
	}
	s.Hops = append(s.Hops, h)
	s.Recompute()
	return s
}

// One spike must not flatten the picture. Scaled to the worst value drawn, a
// two-second stall puts every ordinary column on the floor block and holds it
// there for as long as the spike is in the window - which on a path that spikes
// every few seconds is what "the graph stopped updating" looks like.
func TestOneSpikeDoesNotFlattenTheOtherRows(t *testing.T) {
	v := View{Screen: testScreen(t, 120, 30, true), Interval: time.Second}
	calm := sparkOf(rowFor(v.Live(steadyPair(200, 0)), 1))
	if calm == "" {
		t.Fatal("no series drawn")
	}
	if levels := len(distinctRunes(calm)); levels < 3 {
		t.Fatalf("the reference row is already flat, so this test proves nothing: %s", calm)
	}
	spiked := sparkOf(rowFor(v.Live(steadyPair(200, 2000)), 1))
	if spiked != calm {
		t.Errorf("a spike at another hop rescaled this one:\n  without %s\n  with    %s", calm, spiked)
	}
	// And the spike itself still reads as off the top of the scale.
	if row := sparkOf(rowFor(v.Live(steadyPair(200, 2000)), 2)); !strings.ContainsRune(row, '█') {
		t.Errorf("the spike is not drawn at full height: %s", row)
	}
}

func distinctRunes(s string) map[rune]bool {
	out := map[rune]bool{}
	for _, r := range s {
		out[r] = true
	}
	return out
}
