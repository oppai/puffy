package tui

import (
	"fmt"
	"strings"

	"github.com/oppai/puffy/internal/model"
)

// View renders a session snapshot to terminal lines. One type serves both
// modes: a ping is a trace with a single hop, and the only real difference is
// that a single hop has room for a full line chart.
type View struct {
	Screen *Screen
	// Window is how many trailing rounds to show, or 0 for "as many as fit".
	Window int
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

	out := []string{v.header(c, s, cols), ""}
	out = append(out, v.pingStats(c, hop)...)
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

	// One dot column per sample keeps the curve honest; braille gives two per
	// character, so the window is twice the plot width.
	plotW := cols - 10
	window := v.Window
	if window <= 0 {
		window = plotW * 2
	}
	samples := hop.Window(window)
	spanMS := float64(len(samples)) * s.IntervalMS

	out = append(out, c.Dim("  rtt ms"))
	chart := LineChart{Width: cols - 2, Height: chartRows}
	for _, l := range chart.Render(c, samples, spanMS) {
		out = append(out, "  "+l)
	}

	out = append(out, "")
	// indent(2) + "loss  "(6) + strip + gap(2) + "100.0%"(6)
	pct := fmt.Sprintf("%.1f%%", hop.Stats.LossPct)
	stripW := cols - 2 - 6 - 2 - len(pct)
	if stripW > 4 {
		out = append(out, "  "+c.Dim("loss  ")+LossStrip(c, samples, stripW)+
			"  "+c.Paint(lossRole(hop.Stats.LossPct), pct))
	}
	out = append(out, "")
	legend := "  " + c.Dim("· delivered  ") + c.Paint(RoleCritical, "×") + c.Dim(" lost")
	if cols >= 86 {
		legend += c.Dim("   vertical range = min–max when a column holds several probes")
	}
	out = append(out, legend)
	return fit(out, cols, rows)
}

func (v View) pingStats(c Color, hop *model.Hop) []string {
	st := hop.Stats
	left := fmt.Sprintf("%d sent · %d recv · %d lost", st.Sent, st.Recv, st.Lost)
	loss := c.Paint(lossRole(st.LossPct), fmt.Sprintf("loss %.1f%%", st.LossPct))

	row1 := fmt.Sprintf("  %-28s %s   last %sms   avg %sms",
		left, loss, fmtMS(st.Last), fmtMS(st.Avg))
	row2 := fmt.Sprintf("  %-28s %s", "",
		c.Dim(fmt.Sprintf("best %sms   worst %sms   p95 %sms   jitter %sms",
			fmtMS(st.Best), fmtMS(st.Worst), fmtMS(st.P95), fmtMS(st.Jitter))))
	return []string{row1, row2}
}

// --- trace ------------------------------------------------------------------

func (v View) traceFrame(c Color, s *model.Session, cols, rows int, summary bool) []string {
	if len(s.Hops) == 0 {
		return []string{v.header(c, s, cols), "", c.Dim("  discovering the path…")}
	}

	out := []string{v.header(c, s, cols), ""}

	lay := layoutFor(cols)
	plan := s.PlanCols(lay.sparkW, v.Window)
	scaleMax := s.MaxRTT()
	spanMS := float64(plan.LastRound-plan.FirstRound+1) * s.IntervalMS

	head := fmt.Sprintf("%3s %-*s %6s", "ttl", lay.hostW, "host", "loss")
	for _, sc := range lay.stats {
		head += fmt.Sprintf(" %7s", sc.head)
	}
	out = append(out, c.Dim(head+"  "+"rtt over time →"))

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

	cellsFor := make(map[int][]model.Cell, len(s.Hops))
	for _, h := range s.Hops {
		cellsFor[h.TTL] = h.Cells(plan)
	}

	for i, h := range s.Hops {
		if i >= maxHops {
			out = append(out, c.Dim(fmt.Sprintf("    … %d more hops (widen or lengthen the window)", len(s.Hops)-maxHops)))
			break
		}
		out = append(out, v.hopRow(c, h, lay, cellsFor[h.TTL], scaleMax))
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

func (v View) hopRow(c Color, h *model.Hop, lay layout, cells []model.Cell, scaleMax float64) string {
	st := h.Stats
	name := h.Label()
	if h.Final {
		name = "● " + name // the destination
	}
	label := truncate(name, lay.hostW)

	lossCell := c.Paint(lossRole(st.LossPct), fmt.Sprintf("%5.1f%%", st.LossPct))
	if st.Sent == 0 {
		lossCell = c.Dim("     -")
	}

	var nums string
	for _, sc := range lay.stats {
		if st.Recv == 0 {
			nums += c.Dim(fmt.Sprintf(" %7s", "-"))
			continue
		}
		nums += fmt.Sprintf(" %7s", fmtCol(sc.val(st)))
	}

	return fmt.Sprintf("%s %s %s%s  %s",
		c.Dim(fmt.Sprintf("%3d", h.TTL)),
		fmt.Sprintf("%-*s", lay.hostW, label),
		lossCell, nums,
		Sparkline(c, cells, scaleMax, h.Baseline()))
}

// layout is the responsive column plan for the hop table.
type layout struct {
	hostW  int
	sparkW int
	stats  []statCol
}

type statCol struct {
	head string
	val  func(model.Stats) float64
}

// statSets are the candidate statistic columns, widest first. A narrow terminal
// drops numbers rather than the time series: the series is the point of the
// tool, and the numbers are all in the JSON and the HTML table anyway.
var statSets = [][]statCol{
	{
		{"last", func(s model.Stats) float64 { return s.Last }},
		{"avg", func(s model.Stats) float64 { return s.Avg }},
		{"best", func(s model.Stats) float64 { return s.Best }},
		{"worst", func(s model.Stats) float64 { return s.Worst }},
	},
	{
		{"last", func(s model.Stats) float64 { return s.Last }},
		{"avg", func(s model.Stats) float64 { return s.Avg }},
		{"worst", func(s model.Stats) float64 { return s.Worst }},
	},
	{
		{"avg", func(s model.Stats) float64 { return s.Avg }},
		{"worst", func(s model.Stats) float64 { return s.Worst }},
	},
	{
		{"avg", func(s model.Stats) float64 { return s.Avg }},
	},
	{},
}

// layoutFor picks the widest column set that still leaves room for a hostname
// and a usable sparkline.
func layoutFor(cols int) layout {
	// ttl(3) + gap + host + gap + loss(6) + stats + two gaps + spark
	const chrome = 3 + 1 + 1 + 6 + 2
	const minHost, minSpark = 12, 8

	for _, set := range statSets {
		rest := cols - chrome - 8*len(set)
		if rest < minHost+minSpark {
			continue
		}
		hostW := min(28, max(minHost, rest/3))
		return layout{hostW: hostW, sparkW: rest - hostW, stats: set}
	}
	// Narrower than any sane terminal: keep the invariant rather than panicking.
	rest := max(cols-chrome, minHost+2)
	hostW := max(min(12, rest-2), 1)
	return layout{hostW: hostW, sparkW: max(rest-hostW, 1)}
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

// panels renders the two heatmaps that answer "where" and "when". Only hops
// that actually show something are listed: a wall of empty rows buries the two
// that matter.
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

	lossRows := make([]string, 0, len(s.Hops))
	for _, h := range s.Hops {
		if h.Stats.Lost == 0 {
			continue
		}
		lossRows = append(lossRows, fmt.Sprintf("%s %s %s",
			c.Dim(fmt.Sprintf("%3d", h.TTL)),
			fmt.Sprintf("%-*s", labelW, truncate(h.Label(), labelW)),
			HeatRow(c, cellsFor[h.TTL], HeatLoss, 0)))
	}
	// Each panel gets half of what is left after its own title and the closing
	// note, so a lossy path cannot squeeze the stall panel off the screen.
	perPanel := max((budget-3)/2, 1)

	if len(lossRows) == 0 {
		out = append(out, c.Dim("  packet loss")+"  "+c.Paint(RoleGood, "none")+
			c.Dim(fmt.Sprintf("  ·  %s of history", fmtSpan(spanMS))))
	} else {
		out = append(out, c.Dim("  "+title("packet loss — where it lands, and when", "packet loss"))+"   "+
			HeatLegend(c, HeatLoss, "0%", "100%"))
		out = append(out, elide(c, lossRows, perPanel)...)
	}

	stallRows := make([]string, 0, len(s.Hops))
	for _, h := range s.Hops {
		base := h.Baseline()
		if base <= 0 || h.Stats.Worst < base*warnFactor && h.Stats.Worst < base+warnFloorMS {
			continue
		}
		stallRows = append(stallRows, fmt.Sprintf("%s %s %s",
			c.Dim(fmt.Sprintf("%3d", h.TTL)),
			fmt.Sprintf("%-*s", labelW, truncate(h.Label(), labelW)),
			HeatRow(c, cellsFor[h.TTL], HeatStall, base)))
	}
	out = append(out, "")
	if len(stallRows) == 0 {
		out = append(out, c.Dim("  latency stalls")+"  "+c.Paint(RoleGood, "none"))
	} else {
		out = append(out, c.Dim("  "+title("latency stalls — rtt above each hop's own baseline", "latency stalls"))+"   "+
			HeatLegend(c, HeatStall, "1.25×", "4×+"))
		out = append(out, elide(c, stallRows, perPanel)...)
	}

	if plan.PerCol > 1 {
		out = append(out, "", c.Dim(fmt.Sprintf("  %s of history, %d rounds per column (worst wins)", fmtSpan(spanMS), plan.PerCol)))
	} else {
		out = append(out, "", c.Dim(fmt.Sprintf("  %s of history, 1 round per column", fmtSpan(spanMS))))
	}
	return out
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
	meta := fmt.Sprintf("round %d · every %s · timeout %s",
		s.Rounds, fmtSpan(s.IntervalMS), fmtSpan(s.TimeoutMS))

	// Shed detail from the right rather than letting the line be cut: the
	// target is what the reader needs, the timeout is not.
	full := fmt.Sprintf("puffy %s %s  %s", s.Mode, target, meta)
	if len(full) <= cols {
		return c.Bold("puffy "+s.Mode) + " " + c.Paint(RoleSecondary, target) + "  " + c.Dim(meta)
	}
	short := fmt.Sprintf("round %d · every %s", s.Rounds, fmtSpan(s.IntervalMS))
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
