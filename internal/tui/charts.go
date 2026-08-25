package tui

import (
	"fmt"
	"math"
	"strings"

	"github.com/oppai/puffy/internal/model"
)

// blocks are the eight partial-height levels used by the per-hop sparkline.
var blocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// LineChart draws a latency time series with a labelled y axis.
//
// The y axis does not start at zero. A latency floor of 20ms with a 24ms
// typical value would otherwise flatten into a single row, hiding the variation
// that matters; both bounds are labelled, so the reader is never asked to infer
// the baseline. This is a line chart, not bars - the mark does not encode
// magnitude by its area, so a non-zero origin does not misstate anything.
type LineChart struct {
	Width  int // total columns available, gutter included
	Height int // character rows in the plot area
}

// Render returns the chart rows: the plot, then the x axis rule, then the time
// labels. samples must be in time order; lost probes are breaks in the line,
// never interpolated across.
func (lc LineChart) Render(c Color, samples []model.Sample, spanMS float64) []string {
	rows := lc.Height
	if rows < 2 {
		rows = 2
	}

	lo, hi, ticks := niceBounds(samples)
	if math.IsNaN(lo) {
		return []string{c.Dim("  no replies yet")}
	}

	// Gutter is as wide as the widest tick label.
	gutter := 0
	for _, t := range ticks {
		if n := len(fmtMS(t)); n > gutter {
			gutter = n
		}
	}
	gutter += 1 // one space between label and axis

	// The end label rides the last point, so reserve room for it.
	endLabel := ""
	if last, ok := lastReply(samples); ok {
		endLabel = " " + fmtMS(last) + "ms"
	}

	plotW := lc.Width - gutter - 1 - len(endLabel)
	if plotW < 8 {
		plotW = lc.Width - gutter - 1
		endLabel = ""
	}
	if plotW < 4 {
		return []string{c.Dim("  terminal too narrow for the chart")}
	}

	canvas := NewBraille(plotW, rows)
	dotW, dotH := canvas.DotW(), canvas.DotH()

	yOf := func(v float64) int {
		frac := (v - lo) / (hi - lo)
		y := int(math.Round((1 - frac) * float64(dotH-1)))
		return min(max(y, 0), dotH-1)
	}

	// A point is one mark on the plot. lost points break the line; a column
	// that simply has no sample in it does not - that distinction is the whole
	// difference between "the probe was dropped" and "the canvas is wider than
	// the history", and conflating them draws a solid series as a dotted one.
	type point struct {
		x, y     int
		lost     bool
		band     bool
		loY, hiY int
	}
	var points []point

	if len(samples) <= dotW {
		// One mark per sample, spread across the canvas.
		for i, s := range samples {
			x := 0
			if len(samples) > 1 {
				x = i * (dotW - 1) / (len(samples) - 1)
			}
			if s.Lost() {
				points = append(points, point{x: x, lost: true})
				continue
			}
			points = append(points, point{x: x, y: yOf(*s.RTTMS)})
		}
	} else {
		// More history than resolution: fold into columns. Each column carries
		// the mean as the line and the min-max spread as a vertical range, so a
		// one-off spike is never averaged away. A column counts as lost only
		// when every probe in it was lost.
		type col struct {
			min, max, sum float64
			n, lost       int
		}
		cols := make([]col, dotW)
		for i := range cols {
			cols[i].min, cols[i].max = math.Inf(1), math.Inf(-1)
		}
		for i, s := range samples {
			x := i * (dotW - 1) / (len(samples) - 1)
			if s.Lost() {
				cols[x].lost++
				continue
			}
			v := *s.RTTMS
			cols[x].n++
			cols[x].sum += v
			cols[x].min = math.Min(cols[x].min, v)
			cols[x].max = math.Max(cols[x].max, v)
		}
		for x := range cols {
			switch {
			case cols[x].n > 0:
				p := point{x: x, y: yOf(cols[x].sum / float64(cols[x].n))}
				if cols[x].max > cols[x].min {
					p.band, p.loY, p.hiY = true, yOf(cols[x].min), yOf(cols[x].max)
				}
				points = append(points, p)
			case cols[x].lost > 0:
				points = append(points, point{x: x, lost: true})
			}
			// A column with neither is simply empty and is skipped, so the
			// neighbours on either side still connect.
		}
	}

	prev := -1
	for i, p := range points {
		if p.lost {
			prev = -1 // a real gap: do not bridge it
			continue
		}
		if p.band {
			canvas.VLine(p.x, p.loY, p.hiY)
		}
		if prev >= 0 {
			canvas.Line(points[prev].x, points[prev].y, p.x, p.y)
		} else {
			canvas.Set(p.x, p.y)
		}
		prev = i
	}

	// The direct label rides the row the series actually ends on. Pinning it to
	// the top row would read as a label for the top of the axis.
	endRow := 0
	for i := len(points) - 1; i >= 0; i-- {
		if !points[i].lost {
			endRow = points[i].y / 4
			break
		}
	}

	// Compose. Tick rows get a value and a stub; the rest get a bare axis.
	tickRow := map[int]float64{}
	for _, t := range ticks {
		r := yOf(t) / 4
		if _, taken := tickRow[r]; !taken {
			tickRow[r] = t
		}
	}

	body := canvas.Rows()
	out := make([]string, 0, rows+3)
	for r := range rows {
		label := strings.Repeat(" ", gutter)
		axis := "│"
		if t, ok := tickRow[r]; ok {
			label = fmt.Sprintf("%*s ", gutter-1, fmtMS(t))
			axis = "┤"
		}
		line := c.Dim(label) + c.Paint(RoleAxis, axis) + c.Paint(RoleSeries, body[r])
		if r == endRow && endLabel != "" {
			// Label the current value once, where the line ends, instead of
			// annotating every point.
			pad := max(plotW-visibleLen(body[r]), 1)
			line += strings.Repeat(" ", pad) + c.Paint(RoleSecondary, strings.TrimSpace(endLabel))
		}
		out = append(out, line)
	}

	out = append(out,
		c.Dim(strings.Repeat(" ", gutter))+c.Paint(RoleAxis, "└"+strings.Repeat("─", plotW)))
	out = append(out, c.Dim(strings.Repeat(" ", gutter+1)+xAxisLabels(plotW, spanMS)))
	return out
}

// lastReply returns the most recent non-lost RTT.
func lastReply(samples []model.Sample) (float64, bool) {
	for i := len(samples) - 1; i >= 0; i-- {
		if !samples[i].Lost() {
			return *samples[i].RTTMS, true
		}
	}
	return 0, false
}

// niceBounds picks axis bounds and up to three round tick values that bracket
// the data. It returns NaN bounds when there is nothing to plot.
func niceBounds(samples []model.Sample) (lo, hi float64, ticks []float64) {
	minV, maxV := math.Inf(1), math.Inf(-1)
	for _, s := range samples {
		if s.Lost() {
			continue
		}
		minV = math.Min(minV, *s.RTTMS)
		maxV = math.Max(maxV, *s.RTTMS)
	}
	if math.IsInf(minV, 1) {
		return math.NaN(), math.NaN(), nil
	}
	if maxV-minV < 1 { // a dead-flat series still needs a visible band
		pad := math.Max(maxV*0.1, 0.5)
		minV, maxV = minV-pad, maxV+pad
	}
	span := maxV - minV
	// Quarter of the span, not half: a coarser step rounds the bounds so far
	// out that the series occupies a fraction of the available height.
	step := niceStep(span / 4)
	lo = math.Floor(minV/step) * step
	hi = math.Ceil(maxV/step) * step
	if hi <= lo {
		hi = lo + step
	}
	for v := lo; v <= hi+step/2; v += step {
		ticks = append(ticks, v)
	}
	// Three labels - floor, middle, ceiling - is all a terminal has room for.
	if len(ticks) > 3 {
		mid := ticks[len(ticks)/2]
		ticks = []float64{ticks[0], mid, ticks[len(ticks)-1]}
	}
	return lo, hi, ticks
}

// niceStep rounds a raw interval up to the nearest 1, 2 or 5 times a power of
// ten, so axis labels read as round numbers.
func niceStep(raw float64) float64 {
	if raw <= 0 {
		return 1
	}
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	switch n := raw / mag; {
	case n <= 1:
		return mag
	case n <= 2:
		return 2 * mag
	case n <= 5:
		return 5 * mag
	default:
		return 10 * mag
	}
}

// xAxisLabels lays "-60s ... -30s ... now" across the plot width.
func xAxisLabels(width int, spanMS float64) string {
	if width < 12 || spanMS <= 0 {
		return ""
	}
	left := "-" + fmtSpan(spanMS)
	mid := "-" + fmtSpan(spanMS/2)
	right := "now"
	if width < len(left)+len(mid)+len(right)+6 {
		mid = ""
	}
	line := make([]byte, width)
	for i := range line {
		line[i] = ' '
	}
	copy(line, left)
	if mid != "" {
		copy(line[(width-len(mid))/2:], mid)
	}
	copy(line[width-len(right):], right)
	return string(line)
}

// LossStrip is the companion row under a latency chart: one mark per dot column
// where a probe was lost. Loss is a separate row rather than a gap in the line
// because a gap and a flat segment look alike at terminal resolution.
func LossStrip(c Color, samples []model.Sample, width int) string {
	if width < 1 {
		return ""
	}
	marks := make([]bool, width)
	any := false
	for i, s := range samples {
		if !s.Lost() {
			continue
		}
		x := 0
		if len(samples) > 1 {
			x = i * (width - 1) / (len(samples) - 1)
		}
		marks[x] = true
		any = true
	}
	if !any {
		return c.Dim(strings.Repeat("·", width))
	}
	var sb strings.Builder
	for _, m := range marks {
		if m {
			sb.WriteString(c.Paint(RoleCritical, "×"))
		} else {
			sb.WriteString(c.Dim("·"))
		}
	}
	return sb.String()
}

// Stall thresholds, expressed against a hop's own baseline. A hop that normally
// answers in 1ms and now takes 4ms is not a problem; the absolute floor keeps
// LAN jitter from lighting up the whole table.
const (
	warnFactor  = 1.8
	warnFloorMS = 8.0
	badFactor   = 2.5
	badFloorMS  = 15.0

	// Bounds of the stall heatmap's colour scale, as a multiple of a hop's own
	// baseline. Below stallFloorX a cell reads as nothing.
	stallFloorX = 1.25
	stallTopX   = 4.0
)

// Sparkline renders one hop's folded time series. Height encodes RTT on a
// scale shared by every hop, so rows are comparable; colour encodes state
// against that hop's own baseline, so a hop that is stalling relative to itself
// still stands out on a path where some other hop is slower in absolute terms.
// The two channels answer different questions and neither restates the other.
//
// When several rounds share a column the worst RTT wins, because a spike that
// gets averaged away is a spike the operator never sees.
func Sparkline(c Color, cells []model.Cell, scaleMax, baseline float64) string {
	if scaleMax <= 0 {
		scaleMax = 1
	}
	warn := math.Max(baseline*warnFactor, baseline+warnFloorMS)
	bad := math.Max(baseline*badFactor, baseline+badFloorMS)

	var sb strings.Builder
	for _, cell := range cells {
		switch {
		case cell.Empty():
			sb.WriteByte(' ')
		case cell.Lost == cell.Sent:
			sb.WriteString(c.Paint(RoleCritical, "×"))
		default:
			v := cell.MaxRTT
			level := int(math.Ceil(v / scaleMax * float64(len(blocks))))
			level = min(max(level, 1), len(blocks))
			glyph := string(blocks[level-1])

			role := RoleSeries
			switch {
			case cell.Lost > 0:
				// Partial loss keeps the height but takes the loss colour, so
				// the glyph says how slow and the hue says it also dropped.
				role = RoleCritical
			case baseline > 0 && v >= bad:
				role = RoleSerious
			case baseline > 0 && v >= warn:
				role = RoleWarning
			}
			sb.WriteString(c.Paint(role, glyph))
		}
	}
	return sb.String()
}

// HeatKind selects which magnitude a heat row encodes.
type HeatKind int

const (
	// HeatLoss encodes the share of probes lost in each cell.
	HeatLoss HeatKind = iota
	// HeatStall encodes how far above its own baseline a hop sat.
	HeatStall
)

// heatClasses are the four ordinal bins. The glyph is a second, colour-free
// channel carrying the same order, so the heatmap still reads under NO_COLOR,
// in a screenshot printed in grey, or with any colour vision.
var heatClasses = []struct {
	upTo  float64
	glyph string
	step  int // index into the ordinal ramp
}{
	{0.10, "░", 0},
	{0.35, "▒", 1},
	{0.70, "▓", 2},
	{math.Inf(1), "█", 4},
}

// HeatRow renders one hop's row of a heatmap. ref is the hop baseline for
// HeatStall and is ignored for HeatLoss.
func HeatRow(c Color, cells []model.Cell, kind HeatKind, ref float64) string {
	ramp := LossRamp
	if kind == HeatStall {
		ramp = LatRamp
	}
	var sb strings.Builder
	for _, cell := range cells {
		frac := 0.0
		switch {
		case cell.Empty():
			sb.WriteByte(' ')
			continue
		case kind == HeatLoss:
			frac = cell.LossPct / 100
		case ref > 0:
			// Ordinary jitter puts almost every bucket a little above its own
			// p20, so the ramp only starts once the inflation is meaningful.
			// Without that dead zone the panel fills in solid and hides the
			// hotspots it exists to show. Four times baseline saturates it:
			// past that the exact multiple stops mattering.
			frac = (cell.MaxRTT/ref - stallFloorX) / (stallTopX - stallFloorX)
		}
		if frac <= 0 {
			sb.WriteString(c.Paint(RoleAxis, "·"))
			continue
		}
		for _, cl := range heatClasses {
			if frac <= cl.upTo {
				sb.WriteString(c.Paint(ramp[cl.step], cl.glyph))
				break
			}
		}
	}
	return sb.String()
}

// HeatLegend describes the ordinal bins in reading order.
func HeatLegend(c Color, kind HeatKind, lowLabel, highLabel string) string {
	ramp := LossRamp
	if kind == HeatStall {
		ramp = LatRamp
	}
	var sb strings.Builder
	sb.WriteString(c.Dim(lowLabel + " "))
	sb.WriteString(c.Paint(RoleAxis, "·"))
	for _, cl := range heatClasses {
		sb.WriteString(c.Paint(ramp[cl.step], cl.glyph))
	}
	sb.WriteString(c.Dim(" " + highLabel))
	return sb.String()
}

// fmtMS formats a millisecond value at a precision that suits its magnitude:
// sub-millisecond LAN hops need two decimals, a 400ms satellite hop needs none.
func fmtMS(v float64) string {
	switch {
	case v <= 0:
		return "0"
	case v < 1:
		return fmt.Sprintf("%.2f", v)
	case v < 10:
		return fmt.Sprintf("%.1f", v)
	case v < 1000:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

// fmtCol formats a millisecond value for a table column. Unlike fmtMS it keeps
// the decimal count fixed, because a right-aligned column that mixes "2.1" and
// "25" reads as though the two were measured differently.
func fmtCol(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// fmtSpan renders a millisecond duration as a compact label. Sub-second values
// keep their milliseconds: a 300ms probe interval rounded to seconds reads as
// "0s", which is worse than no label at all.
func fmtSpan(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", int(math.Round(ms)))
	}
	sec := ms / 1000
	switch {
	case sec < 10 && math.Mod(sec, 1) > 0.05:
		return fmt.Sprintf("%.1fs", sec)
	case sec < 60:
		return fmt.Sprintf("%ds", int(math.Round(sec)))
	case sec < 3600:
		whole := int(math.Round(sec))
		if whole%60 == 0 {
			return fmt.Sprintf("%dm", whole/60)
		}
		return fmt.Sprintf("%dm%ds", whole/60, whole%60)
	default:
		whole := int(math.Round(sec))
		return fmt.Sprintf("%dh%dm", whole/3600, (whole%3600)/60)
	}
}

// visibleLen counts printable columns, ignoring escape sequences. The braille
// rows are plain text by the time this sees them, so counting runes is enough.
func visibleLen(s string) int { return len([]rune(s)) }
