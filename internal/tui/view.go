package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/oppai/puffy/internal/model"
)

// View renders a session snapshot to terminal lines. One type serves both
// modes: a ping is a trace with a single hop, and the only real difference is
// that a single hop has room for a full line chart.
//
// Every frame is built from two things that must not be confused: the window,
// which is the stretch of time the graphs draw, and the session, which is
// everything measured since the run started. A number is always labelled with
// which of the two it describes.
type View struct {
	Screen *Screen
	// Window is how many trailing rounds to graph, or 0 for the default window.
	Window int
	// Interval is the probe interval, used to size the default window in time
	// rather than in rounds. Zero falls back to the session's own interval.
	Interval time.Duration
}

// liveWindowMS is how much time the live graphs show by default. Fixing the
// window in time - rather than letting it grow to "the whole run so far" - is
// what keeps the picture moving after six hours: a column is worth the same
// number of rounds at minute one and at hour six, so the graph scrolls instead
// of folding itself flat.
const liveWindowMS = 60000

// maxLiveFold caps how many rounds the default window folds into one column.
// Past a couple of hundred a column is a blur - worst-wins folding stops saying
// anything new - while the frame keeps paying for every probe in it. Only an
// interval under about 10ms reaches this; an explicit --window is obeyed as
// given, because then the span is what the reader asked for.
const maxLiveFold = 200

// WindowRounds is how many trailing rounds the next frame will draw. The caller
// snapshots exactly this much history, so one redraw costs the window rather
// than the whole run - which is what keeps a long session's graph moving and its
// Ctrl-C prompt.
func (v View) WindowRounds(mode string) int {
	cols, _ := v.Screen.Size()
	if mode == "ping" {
		return pingWindow(cols, v.Window)
	}
	return traceWindow(layoutFor(cols, statSets).sparkW, v.intervalMS(nil), v.Window)
}

// intervalMS prefers the interval the run was configured with, falling back to
// whatever the session document says.
func (v View) intervalMS(s *model.Session) float64 {
	if v.Interval > 0 {
		return float64(v.Interval) / float64(time.Millisecond)
	}
	if s != nil {
		return s.IntervalMS
	}
	return 0
}

// traceWindow is the trace graph's default window in rounds: the last minute,
// or a screenful of rounds when the interval is so slow that a minute would not
// fill the width.
func traceWindow(sparkW int, intervalMS float64, override int) int {
	if override > 0 {
		return override
	}
	n := sparkW
	if intervalMS > 0 {
		if k := int(liveWindowMS / intervalMS); k > n {
			n = min(k, sparkW*maxLiveFold)
		}
	}
	return max(n, 1)
}

// pingWindow is the ping chart's window: one dot column per probe, and braille
// gives two dots per character.
func pingWindow(cols, override int) int {
	if override > 0 {
		return override
	}
	return max((cols-10)*2, 8)
}

// Live renders the frame for an in-progress session.
func (v View) Live(s *model.Session) []string {
	cols, rows := v.Screen.Size()
	c := v.Screen.Color()
	if s.Mode == "ping" {
		return v.pingFrame(c, s, cols, rows)
	}
	return v.traceFrame(c, s, cols, rows, false)
}

// Summary renders the after-the-fact report, including the insight list. It is
// printed on the normal screen once the live view is torn down.
func (v View) Summary(s *model.Session) []string {
	cols, _ := v.Screen.Size()
	c := v.Screen.Color()
	// No row limit: a summary is allowed to scroll.
	out := v.traceFrame(c, s, cols, 1<<20, true)
	return append(out, fit(v.insights(c, s, cols), cols, 0)...)
}

// --- ping -------------------------------------------------------------------

func (v View) pingFrame(c Color, s *model.Session, cols, rows int) []string {
	hop := s.Dest()
	if hop == nil {
		return []string{c.Dim("waiting for the first reply…")}
	}

	// One dot column per sample keeps the curve honest; braille gives two per
	// character, so the window is twice the plot width.
	samples := hop.Window(pingWindow(cols, v.Window))
	win := model.StatsOf(samples)
	spanMS := float64(len(samples)) * s.IntervalMS

	out := []string{v.header(c, s, cols), ""}
	out = append(out, v.pingStats(c, win, hop.Stats, spanMS, s.Elapsed(), cols)...)
	out = append(out, "")

	// Everything not spent on the chart: header 1, blank 2, stats 2, blank 1,
	// axis 1, x labels 1, blank 1, loss label+strip 1, legend 1.
	chartRows := rows - 12
	if chartRows > 14 {
		chartRows = 14
	}
	if chartRows < 3 {
		chartRows = 3
	}

	out = append(out, c.Dim("  rtt ms"))
	chart := LineChart{Width: cols - 2, Height: chartRows}
	for _, l := range chart.Render(c, samples, spanMS) {
		out = append(out, "  "+l)
	}

	out = append(out, "")
	// indent(2) + "loss  "(6) + strip + gap(2) + "100.0%"(6). The percentage is
	// the window's, because the strip beside it is the window.
	pct := fmt.Sprintf("%.1f%%", win.LossPct)
	stripW := cols - 2 - 6 - 2 - len(pct)
	if stripW > 4 {
		out = append(out, "  "+c.Dim("loss  ")+LossStrip(c, samples, stripW)+
			"  "+c.Paint(lossRole(win.LossPct), pct))
	}
	out = append(out, "")
	legend := "  " + c.Dim("· delivered  ") + c.Paint(RoleCritical, "×") + c.Dim(" lost")
	if cols >= 86 {
		legend += c.Dim("   vertical range = min–max when a column holds several probes")
	}
	out = append(out, legend)
	return fit(out, cols, rows)
}

// pingStats prints the window on one line and the session on the next. Two rows
// rather than one set of numbers, because "12ms average" means something
// different over the last minute than over the last six hours, and the row that
// answers "is it bad right now" is not the row that answers "has it been bad".
func (v View) pingStats(c Color, win, sess model.Stats, winSpan, sessSpan float64, cols int) []string {
	row := func(label string, st model.Stats, fields []string, dim bool) string {
		head := "  " + c.Dim(fmt.Sprintf("%-14s", label))
		loss := c.Paint(lossRole(st.LossPct), fmt.Sprintf("loss %5.1f%%", st.LossPct))
		if st.Sent == 0 {
			loss = c.Dim("loss      -")
		}
		// 2 indent + 14 label + 11 loss + 3 gap
		tail := joinFit(fields, cols-30)
		if dim {
			tail = c.Dim(tail)
		}
		return head + loss + "   " + tail
	}

	winFields := []string{
		fmt.Sprintf("%d/%d recv", win.Recv, win.Sent),
		"last " + fmtMS(win.Last) + "ms",
		"avg " + fmtMS(win.Avg) + "ms",
		"best " + fmtMS(win.Best),
		"worst " + fmtMS(win.Worst),
		"jitter " + fmtMS(win.Jitter),
	}
	sessFields := []string{
		fmt.Sprintf("%d/%d recv", sess.Recv, sess.Sent),
		"avg " + fmtMS(sess.Avg) + "ms",
		"p50 " + fmtMS(sess.P50),
		"p95 " + fmtMS(sess.P95),
		"best " + fmtMS(sess.Best),
		"worst " + fmtMS(sess.Worst),
	}
	return []string{
		row("window "+fmtSpan(winSpan), win, winFields, false),
		row("session "+fmtSpan(sessSpan), sess, sessFields, true),
	}
}

// joinFit joins as many fields as fit in w columns, so a narrow terminal sheds
// the least important number instead of having a value cut in half.
func joinFit(fields []string, w int) string {
	var out string
	for _, f := range fields {
		next := f
		if out != "" {
			next = out + "   " + f
		}
		if len(next) > w {
			break
		}
		out = next
	}
	return out
}

// --- trace ------------------------------------------------------------------

func (v View) traceFrame(c Color, s *model.Session, cols, rows int, summary bool) []string {
	if len(s.Hops) == 0 {
		return []string{v.header(c, s, cols), "", c.Dim("  discovering the path…")}
	}

	out := []string{v.header(c, s, cols), ""}

	sets := statSets
	if summary {
		sets = summarySets
	}
	lay := layoutFor(cols, sets)
	// The live view graphs a fixed window and scrolls; the summary is allowed to
	// fold everything it still holds into the same width, because by then the
	// whole run is the thing being reported.
	window := 0
	if !summary {
		window = traceWindow(lay.sparkW, v.intervalMS(s), v.Window)
	} else if v.Window > 0 {
		window = v.Window
	}
	plan := s.PlanCols(lay.sparkW, window)
	spanMS := float64(plan.LastRound-plan.FirstRound+1) * s.IntervalMS

	cellsFor := make(map[int][]model.Cell, len(s.Hops))
	winStats := make(map[int]model.Stats, len(s.Hops))
	windowed := lay.hasScope(scopeWindow)
	for _, h := range s.Hops {
		cellsFor[h.TTL] = h.Cells(plan)
		if windowed {
			winStats[h.TTL] = model.StatsOf(h.Range(plan))
		}
	}
	scaleMax := scaleOf(s, cellsFor)

	out = append(out, lay.groupHeader(c, spanMS, s.Elapsed()))
	out = append(out, c.Dim(lay.head()+"  "+"rtt over time →"))

	// Reserve room for the panels below before deciding how many hops fit.
	reserve := 6
	if summary {
		reserve = 0
	}
	maxHops := len(s.Hops)
	if !summary {
		if avail := rows - len(out) - reserve; avail < maxHops {
			maxHops = max(avail, 1)
		}
	}

	for i, h := range s.Hops {
		if i >= maxHops {
			out = append(out, c.Dim(fmt.Sprintf("    … %d more hops (widen or lengthen the window)", len(s.Hops)-maxHops)))
			break
		}
		out = append(out, v.hopRow(c, h, lay, cellsFor[h.TTL], winStats[h.TTL], scaleMax))
	}

	// Whatever is left after the hop table is the panels' budget. Below four
	// rows there is no room for a titled panel, so the table keeps the space.
	budget := rows - len(out) - 1
	if !summary && budget < 4 {
		return fit(out, cols, rows)
	}
	if summary {
		budget = 1 << 20
	}

	out = append(out, "")
	out = append(out, v.panels(c, s, plan, cellsFor, lay, cols, spanMS, budget)...)
	return fit(out, cols, rows)
}

// scaleOf is the shared vertical scale for the sparklines: the worst RTT drawn
// in this frame. Scaling to the whole session instead would flatten every row
// the moment one spike landed, and that one spike would then keep the graph flat
// for the rest of the run - a long session's series would slowly die out.
func scaleOf(s *model.Session, cellsFor map[int][]model.Cell) float64 {
	var m float64
	for _, cells := range cellsFor {
		for _, cell := range cells {
			if cell.MaxRTT > m {
				m = cell.MaxRTT
			}
		}
	}
	if m <= 0 {
		return s.MaxRTT()
	}
	return m
}

func (v View) hopRow(c Color, h *model.Hop, lay layout, cells []model.Cell, win model.Stats, scaleMax float64) string {
	name := h.Label()
	if h.Final {
		name = "● " + name // the destination
	}
	label := truncate(name, lay.hostW)

	stats := lay.block(func(col statCol) string {
		st := win
		if col.scope == scopeSession {
			st = h.Stats
		}
		switch {
		case st.Sent == 0, !col.pct && st.Recv == 0:
			return c.Dim(fmt.Sprintf("%*s", statW, "-"))
		case col.pct:
			return c.Paint(lossRole(st.LossPct), fmt.Sprintf("%*.1f%%", statW-1, st.LossPct))
		default:
			return fmt.Sprintf("%*s", statW, fmtCol(col.val(st)))
		}
	})

	return fmt.Sprintf("%s %s%s  %s",
		c.Dim(fmt.Sprintf("%3d", h.TTL)),
		fmt.Sprintf("%-*s", lay.hostW, label),
		stats,
		Sparkline(c, cells, scaleMax, h.Baseline()))
}

// --- table layout -----------------------------------------------------------

// scope says which statistics a column reads. Keeping the two apart is the
// point of the table: a session average over six hours cannot tell you the path
// went bad thirty seconds ago, and a thirty-second average cannot tell you it
// has been going bad all day.
type scope int

const (
	scopeWindow scope = iota
	scopeSession
)

// statW is the width of every statistic column. One width for all of them means
// the group rules above the table line up with the columns they cover.
const statW = 7

type statCol struct {
	head  string
	scope scope
	pct   bool // a loss percentage, coloured by severity
	val   func(model.Stats) float64
}

func lossCol(sc scope) statCol {
	return statCol{head: "loss", scope: sc, pct: true}
}

func msCol(head string, sc scope, val func(model.Stats) float64) statCol {
	return statCol{head: head, scope: sc, val: val}
}

func statLast(s model.Stats) float64  { return s.Last }
func statBest(s model.Stats) float64  { return s.Best }
func statAvg(s model.Stats) float64   { return s.Avg }
func statWorst(s model.Stats) float64 { return s.Worst }
func statP95(s model.Stats) float64   { return s.P95 }

// statSets are the candidate column sets, widest first. A narrow terminal drops
// numbers rather than the time series - the series is the point of the tool, and
// every number is in the JSON and the HTML table anyway - but it never drops the
// window/session pairing until there is only one column left to keep: knowing
// whether a figure is "now" or "all run" matters more than any of the figures.
var statSets = [][]statCol{
	{
		lossCol(scopeWindow), msCol("last", scopeWindow, statLast),
		msCol("avg", scopeWindow, statAvg), msCol("worst", scopeWindow, statWorst),
		lossCol(scopeSession), msCol("avg", scopeSession, statAvg),
		msCol("p95", scopeSession, statP95), msCol("worst", scopeSession, statWorst),
	},
	{
		lossCol(scopeWindow), msCol("last", scopeWindow, statLast),
		msCol("avg", scopeWindow, statAvg),
		lossCol(scopeSession), msCol("avg", scopeSession, statAvg),
		msCol("worst", scopeSession, statWorst),
	},
	{
		lossCol(scopeWindow), msCol("last", scopeWindow, statLast),
		msCol("avg", scopeWindow, statAvg),
		lossCol(scopeSession), msCol("avg", scopeSession, statAvg),
	},
	{
		lossCol(scopeWindow), msCol("avg", scopeWindow, statAvg),
		lossCol(scopeSession), msCol("avg", scopeSession, statAvg),
	},
	{
		lossCol(scopeWindow), msCol("avg", scopeWindow, statAvg),
		lossCol(scopeSession),
	},
	{lossCol(scopeWindow), lossCol(scopeSession)},
	{lossCol(scopeWindow)},
}

// summarySets are the column sets for the after-the-fact report. By then the
// graph covers the whole run it still holds, so a window column beside the
// session one would only say the same thing twice; the width goes to the time
// series instead.
var summarySets = [][]statCol{
	{
		lossCol(scopeSession), msCol("last", scopeSession, statLast),
		msCol("avg", scopeSession, statAvg), msCol("best", scopeSession, statBest),
		msCol("worst", scopeSession, statWorst),
	},
	{
		lossCol(scopeSession), msCol("last", scopeSession, statLast),
		msCol("avg", scopeSession, statAvg), msCol("worst", scopeSession, statWorst),
	},
	{
		lossCol(scopeSession), msCol("avg", scopeSession, statAvg),
		msCol("worst", scopeSession, statWorst),
	},
	{lossCol(scopeSession), msCol("avg", scopeSession, statAvg)},
	{lossCol(scopeSession)},
}

// layout is the responsive column plan for the hop table.
type layout struct {
	hostW  int
	sparkW int
	stats  []statCol
}

// statsWidth is how many columns a set occupies, including the leading space
// before each column and the wider gap where the meaning changes.
func statsWidth(set []statCol) int {
	w := 0
	for i, col := range set {
		if i > 0 && col.scope != set[i-1].scope {
			w++
		}
		w += 1 + statW
	}
	return w
}

// layoutFor picks the widest column set that still leaves room for a hostname
// and a usable time series.
func layoutFor(cols int, sets [][]statCol) layout {
	// ttl(3) + gap + host + stats + two gaps + spark
	const chrome = 3 + 1 + 2
	const minHost, minSpark = 12, 24

	for _, set := range sets {
		rest := cols - chrome - statsWidth(set)
		if rest < minHost+minSpark {
			continue
		}
		hostW := min(28, max(minHost, rest/3))
		return layout{hostW: hostW, sparkW: rest - hostW, stats: set}
	}
	// Narrower than any sane terminal: keep the invariant rather than panicking.
	set := sets[len(sets)-1]
	rest := max(cols-chrome-statsWidth(set), minHost+2)
	hostW := max(min(12, rest-2), 1)
	return layout{hostW: hostW, sparkW: max(rest-hostW, 1), stats: set}
}

// hasScope reports whether any column reads the given scope.
func (lay layout) hasScope(sc scope) bool {
	for _, col := range lay.stats {
		if col.scope == sc {
			return true
		}
	}
	return false
}

// block lays the statistic columns out, given a function that renders each one.
// The header, the group rules and every hop row go through it, so they cannot
// drift out of alignment.
func (lay layout) block(cell func(col statCol) string) string {
	var b strings.Builder
	for i, col := range lay.stats {
		if i > 0 && col.scope != lay.stats[i-1].scope {
			b.WriteByte(' ') // a wider gap where the meaning changes
		}
		b.WriteByte(' ')
		b.WriteString(cell(col))
	}
	return b.String()
}

// head is the column-name row.
func (lay layout) head() string {
	return fmt.Sprintf("%3s %-*s", "ttl", lay.hostW, "host") +
		lay.block(func(col statCol) string { return fmt.Sprintf("%*s", statW, col.head) })
}

// groupHeader draws a labelled rule over each run of columns, so no reader has
// to guess whether a number means "right now" or "the whole run".
func (lay layout) groupHeader(c Color, winSpanMS, sessSpanMS float64) string {
	if len(lay.stats) == 0 {
		return ""
	}
	type span struct {
		scope    scope
		from, to int
	}
	var spans []span
	x := 0
	for i, col := range lay.stats {
		if i > 0 && col.scope != lay.stats[i-1].scope {
			x++
		}
		x++ // the column's leading space
		if i == 0 || col.scope != lay.stats[i-1].scope {
			spans = append(spans, span{scope: col.scope, from: x, to: x + statW})
		} else {
			spans[len(spans)-1].to = x + statW
		}
		x += statW
	}

	line := []rune(strings.Repeat(" ", x))
	for _, sp := range spans {
		name, spanMS := "window", winSpanMS
		if sp.scope == scopeSession {
			name, spanMS = "session", sessSpanMS
		}
		copy(line[sp.from:sp.to], []rune(rule(name, spanMS, sp.to-sp.from)))
	}
	return strings.Repeat(" ", 3+1+lay.hostW) + c.Dim(string(line))
}

// rule is a group label and, if there is room, a rule drawn out to the width of
// the columns it covers. The span is dropped before the name is: a label that
// says only "session" still tells the reader what the numbers are.
func rule(name string, spanMS float64, w int) string {
	if w <= 0 {
		return ""
	}
	full := name + " " + fmtSpan(spanMS)
	switch {
	case len(full)+1 <= w:
		return full + " " + strings.Repeat("─", w-len(full)-1)
	case len(full) <= w:
		return full
	case len(name) <= w:
		return name
	default:
		return string([]rune(name)[:w])
	}
}

// elide keeps the first n rows and says how many were dropped, so a truncated
// panel never reads as a complete one.
func elide(c Color, rows []string, n int) []string {
	if n < 1 {
		n = 1
	}
	if len(rows) <= n {
		return rows
	}
	if n == 1 {
		return []string{c.Dim(fmt.Sprintf("    … %d hops, too little room to draw them", len(rows)))}
	}
	out := append([]string(nil), rows[:n-1]...)
	return append(out, c.Dim(fmt.Sprintf("    … %d more hops", len(rows)-(n-1))))
}

// fit makes the frame obey the terminal: every line clamped to the width, and
// no more lines than there are rows. The height cut is a backstop - the callers
// budget their own sections - but a live view that scrolls is unreadable, so the
// invariant is enforced here rather than trusted.
func fit(lines []string, cols, rows int) []string {
	if rows > 0 && len(lines) > rows {
		lines = lines[:rows]
	}
	for i, l := range lines {
		lines[i] = Clamp(l, cols)
	}
	return lines
}

// panels renders the two heatmaps that answer "where" and "when". They describe
// the window, like the graph above them: a hop that lost probes an hour ago and
// is clean now would otherwise sit here as a row of blanks, pointing at nothing.
// The hop's whole-run figures are in the session columns, and the findings below
// cover the run.
func (v View) panels(c Color, s *model.Session, plan model.Plan, cellsFor map[int][]model.Cell, lay layout, cols int, spanMS float64, budget int) []string {
	// The heatmap rows reuse the table's ttl and host columns so their cells
	// line up under the sparklines rather than drifting right.
	labelW := lay.hostW
	// Panel titles carry an explanation when there is room for one.
	roomy := cols >= 92
	title := func(long, short string) string {
		if roomy {
			return long
		}
		return short
	}
	var out []string

	panelRow := func(h *model.Hop, cells string) string {
		return fmt.Sprintf("%s %s %s",
			c.Dim(fmt.Sprintf("%3d", h.TTL)),
			fmt.Sprintf("%-*s", labelW, truncate(h.Label(), labelW)),
			cells)
	}

	lossRows := make([]string, 0, len(s.Hops))
	for _, h := range s.Hops {
		if !lostAny(cellsFor[h.TTL]) {
			continue
		}
		lossRows = append(lossRows, panelRow(h, HeatRow(c, cellsFor[h.TTL], HeatLoss, 0)))
	}
	// Each panel gets half of what is left after its own title and the closing
	// note, so a lossy path cannot squeeze the stall panel off the screen.
	perPanel := max((budget-3)/2, 1)

	if len(lossRows) == 0 {
		out = append(out, c.Dim("  packet loss")+"  "+c.Paint(RoleGood, "none")+
			c.Dim(fmt.Sprintf(" in the last %s", fmtSpan(spanMS))))
	} else {
		out = append(out, c.Dim("  "+title("packet loss — where it lands, and when", "packet loss"))+"   "+
			HeatLegend(c, HeatLoss, "0%", "100%"))
		out = append(out, elide(c, lossRows, perPanel)...)
	}

	stallRows := make([]string, 0, len(s.Hops))
	for _, h := range s.Hops {
		base := h.Baseline()
		if !stalledAny(cellsFor[h.TTL], base) {
			continue
		}
		stallRows = append(stallRows, panelRow(h, HeatRow(c, cellsFor[h.TTL], HeatStall, base)))
	}
	out = append(out, "")
	if len(stallRows) == 0 {
		out = append(out, c.Dim("  latency stalls")+"  "+c.Paint(RoleGood, "none")+
			c.Dim(fmt.Sprintf(" in the last %s", fmtSpan(spanMS))))
	} else {
		out = append(out, c.Dim("  "+title("latency stalls — rtt above each hop's own baseline", "latency stalls"))+"   "+
			HeatLegend(c, HeatStall, "1.25×", "4×+"))
		out = append(out, elide(c, stallRows, perPanel)...)
	}

	note := fmt.Sprintf("  %s of history, 1 round per column", fmtSpan(spanMS))
	if plan.PerCol > 1 {
		note = fmt.Sprintf("  %s of history, %d rounds per column (worst wins)", fmtSpan(spanMS), plan.PerCol)
	}
	if s.SamplesFrom > 0 {
		// The session columns still cover every probe; only the drawable time
		// series was capped. Saying which is which is the difference between a
		// bounded tool and a lying one.
		note += fmt.Sprintf("  ·  probe-by-probe history kept from round %d (--history)", s.SamplesFrom)
	}
	out = append(out, "", c.Dim(note))
	return out
}

// lostAny reports whether any drawn column lost a probe.
func lostAny(cells []model.Cell) bool {
	for _, cell := range cells {
		if cell.Lost > 0 {
			return true
		}
	}
	return false
}

// stalledAny reports whether any drawn column sat far enough above the hop's own
// baseline to be worth a panel row.
func stalledAny(cells []model.Cell, base float64) bool {
	if base <= 0 {
		return false
	}
	limit := max(base*warnFactor, base+warnFloorMS)
	for _, cell := range cells {
		if cell.MaxRTT >= limit {
			return true
		}
	}
	return false
}

// --- shared -----------------------------------------------------------------

func (v View) header(c Color, s *model.Session, cols int) string {
	name := s.TargetName
	if name == "" {
		name = s.Target
	}
	target := s.TargetIP
	if name != "" && name != s.TargetIP {
		target = fmt.Sprintf("%s (%s)", name, s.TargetIP)
	}
	// The elapsed time is in the header because it is what the session columns
	// and the findings are averaged over.
	meta := fmt.Sprintf("round %d · %s · every %s · timeout %s",
		s.Rounds, fmtSpan(s.Elapsed()), fmtSpan(s.IntervalMS), fmtSpan(s.TimeoutMS))

	// Shed detail from the right rather than letting the line be cut: the
	// target is what the reader needs, the timeout is not.
	full := fmt.Sprintf("puffy %s %s  %s", s.Mode, target, meta)
	if len(full) <= cols {
		return c.Bold("puffy "+s.Mode) + " " + c.Paint(RoleSecondary, target) + "  " + c.Dim(meta)
	}
	short := fmt.Sprintf("round %d · %s · every %s", s.Rounds, fmtSpan(s.Elapsed()), fmtSpan(s.IntervalMS))
	if len(fmt.Sprintf("puffy %s %s  %s", s.Mode, target, short)) <= cols {
		return c.Bold("puffy "+s.Mode) + " " + c.Paint(RoleSecondary, target) + "  " + c.Dim(short)
	}
	if len("puffy "+s.Mode+" "+target) <= cols {
		return c.Bold("puffy "+s.Mode) + " " + c.Paint(RoleSecondary, target)
	}
	return c.Bold("puffy "+s.Mode) + " " + c.Paint(RoleSecondary, truncate(s.TargetIP, max(cols-len(s.Mode)-7, 3)))
}

func (v View) insights(c Color, s *model.Session, cols int) []string {
	if len(s.Insights) == 0 {
		return nil
	}
	out := []string{"", c.Bold("  what the graph shows")}
	for _, in := range s.Insights {
		role, mark := RoleMuted, "·"
		switch in.Kind {
		case model.InsightLossOnset:
			role, mark = RoleCritical, "✖"
		case model.InsightStall:
			role, mark = RoleSerious, "▲"
		case model.InsightLossWindow:
			role, mark = RoleWarning, "◆"
		case model.InsightICMPLimited:
			role, mark = RoleMuted, "ℹ"
		case model.InsightPathChange:
			role, mark = RoleMuted, "⇄"
		case model.InsightSilent:
			role, mark = RoleMuted, "◌"
		case model.InsightClean:
			role, mark = RoleGood, "✔"
		}
		where := ""
		if in.TTL > 0 {
			where = fmt.Sprintf("ttl %d ", in.TTL)
			if in.Addr != "" {
				where = fmt.Sprintf("ttl %d %s ", in.TTL, in.Addr)
			}
		}
		body := where + in.Detail
		wrapped := wrap(body, max(cols-4, 20))
		out = append(out, "  "+c.Paint(role, mark)+" "+c.Paint(RoleSecondary, where)+c.Dim(strings.TrimPrefix(wrapped[0], where)))
		for _, cont := range wrapped[1:] {
			out = append(out, "    "+c.Dim(cont))
		}
	}
	return out
}

// wrap breaks text on spaces to fit w columns, returning at least one line.
func wrap(text string, w int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	cur := words[0]
	for _, word := range words[1:] {
		if len(cur)+1+len(word) > w {
			lines = append(lines, cur)
			cur = word
			continue
		}
		cur += " " + word
	}
	return append(lines, cur)
}

func lossRole(pct float64) Role {
	switch {
	case pct <= 0:
		return RoleGood
	case pct < 2:
		return RoleWarning
	case pct < 20:
		return RoleSerious
	default:
		return RoleCritical
	}
}

func truncate(s string, w int) string {
	r := []rune(s)
	if w <= 0 {
		return ""
	}
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}
