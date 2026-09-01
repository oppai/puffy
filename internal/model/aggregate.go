package model

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Cell is one column of a hop's time series after the rounds have been folded
// down to the number of columns the display actually has. When a session runs
// longer than the terminal is wide, several rounds share a cell; loss is then a
// rate rather than a yes/no, which is exactly what the heatmap wants.
type Cell struct {
	Col     int     `json:"col"`
	FromMS  float64 `json:"from_ms"`
	ToMS    float64 `json:"to_ms"`
	Sent    int     `json:"sent"`
	Lost    int     `json:"lost"`
	LossPct float64 `json:"loss_pct"`
	AvgRTT  float64 `json:"avg_ms"`
	MaxRTT  float64 `json:"max_ms"`
}

// Empty reports whether no probe fell into this cell at all (as opposed to
// probes that fell in and were lost).
func (c Cell) Empty() bool { return c.Sent == 0 }

// Plan describes how rounds are mapped onto display columns.
type Plan struct {
	Cols       int // number of columns to render
	FirstRound int // first round included (rounds before this scrolled off)
	LastRound  int // last round included, inclusive
	PerCol     int // rounds folded into each column
}

// PlanCols builds a mapping of at most window trailing rounds onto cols columns.
// A window of 0 means "the whole session".
func (s *Session) PlanCols(cols, window int) Plan {
	if cols < 1 {
		cols = 1
	}
	total := s.Rounds
	if total < 1 {
		total = 1
	}
	// The visible span is limited by the window, by how many rounds one screen
	// of columns can hold without folding more than we have to, and by how far
	// back the raw samples still reach on a long run.
	span := s.RetainedRounds()
	if span > total {
		span = total
	}
	if window > 0 && window < span {
		span = window
	}
	perCol := 1
	if span > cols {
		perCol = (span + cols - 1) / cols
	}
	shown := cols * perCol
	if shown > span {
		shown = span
	}
	first := total - shown
	if first < 0 {
		first = 0
	}
	used := (shown + perCol - 1) / perCol
	if used < 1 {
		used = 1
	}
	return Plan{Cols: used, FirstRound: first, LastRound: total - 1, PerCol: perCol}
}

// Col returns the column a round falls into, or -1 when it is off-screen.
func (p Plan) Col(round int) int {
	if round < p.FirstRound || round > p.LastRound {
		return -1
	}
	c := (round - p.FirstRound) / p.PerCol
	if c >= p.Cols {
		return p.Cols - 1
	}
	return c
}

// Cells folds a hop's samples into the plan's columns.
func (h *Hop) Cells(p Plan) []Cell {
	cells := make([]Cell, p.Cols)
	sums := make([]float64, p.Cols)
	for i := range cells {
		cells[i].Col = i
		cells[i].FromMS = math.NaN()
	}
	// Range, not every sample: on a long run the samples outside the plan
	// outnumber the ones in it by orders of magnitude, and a redraw that walks
	// them all is a redraw that gets slower every hour.
	for _, s := range h.Range(p) {
		c := p.Col(s.Round)
		if c < 0 {
			continue
		}
		cell := &cells[c]
		if math.IsNaN(cell.FromMS) {
			cell.FromMS = s.OffsetMS
		}
		cell.ToMS = s.OffsetMS
		cell.Sent++
		if s.Lost() {
			cell.Lost++
			continue
		}
		v := *s.RTTMS
		sums[c] += v
		if v > cell.MaxRTT {
			cell.MaxRTT = v
		}
	}
	for i := range cells {
		if math.IsNaN(cells[i].FromMS) {
			cells[i].FromMS = 0
		}
		if recv := cells[i].Sent - cells[i].Lost; recv > 0 {
			cells[i].AvgRTT = sums[i] / float64(recv)
		}
		if cells[i].Sent > 0 {
			cells[i].LossPct = float64(cells[i].Lost) / float64(cells[i].Sent) * 100
		}
	}
	return cells
}

// Window returns the trailing n samples of a hop, oldest first.
func (h *Hop) Window(n int) []Sample {
	if n <= 0 || n >= len(h.Samples) {
		return h.Samples
	}
	return h.Samples[len(h.Samples)-n:]
}

// Range returns the samples the plan draws, oldest first. Samples are held in
// round order, so this is a subslice rather than a copy: the window statistics
// beside a graph cost the window, not the run.
func (h *Hop) Range(p Plan) []Sample {
	lo := sort.Search(len(h.Samples), func(i int) bool { return h.Samples[i].Round >= p.FirstRound })
	hi := sort.Search(len(h.Samples), func(i int) bool { return h.Samples[i].Round > p.LastRound })
	if lo >= hi {
		return nil
	}
	return h.Samples[lo:hi]
}

// stallFactor is how far above its own baseline a hop's latency must sit before
// puffy calls it a stall. Two-and-a-bit times baseline plus a floor keeps
// sub-millisecond LAN hops from tripping on noise.
const (
	stallFactor  = 2.5
	stallFloorMS = 15.0
	// A hop that loses at least this fraction of probes is a loss candidate.
	lossThresholdPct = 2.0
	// Downstream hops must stay below this to call the loss ICMP rate limiting.
	icmpLimitedSlackPct = 1.0
)

// Analyze fills in Session.Insights: the plain-language reading of the graphs.
// It answers two questions the visualisation poses - which hop is responsible
// for loss, and when the path stalled - and distinguishes real forwarding loss
// from routers that merely deprioritise ICMP replies.
func (s *Session) Analyze() {
	s.Insights = nil
	s.Recompute()

	// Loss attribution. A router that drops traffic makes every hop behind it
	// lose too; a router that merely rate-limits its own ICMP replies does not.
	//
	// The comparison is against the *least* lossy hop downstream, not the worst.
	// One downstream hop answering cleanly is proof that packets are being
	// forwarded, so the loss seen here was only in the replies. Taking the worst
	// instead would let two consecutive rate-limiting routers cover for each
	// other and both get reported as real faults.
	for i, h := range s.Hops {
		if h.Stats.Sent == 0 || h.Stats.LossPct < lossThresholdPct {
			continue
		}
		downstream := math.Inf(1)
		var haveDownstream bool
		for _, d := range s.Hops[i+1:] {
			if d.Stats.Sent == 0 {
				continue
			}
			haveDownstream = true
			downstream = math.Min(downstream, d.Stats.LossPct)
		}
		if !haveDownstream {
			downstream = 0
		}
		switch {
		case h.Stats.Recv == 0 && len(h.Addrs) == 0 && haveDownstream:
			// Nothing ever answered at this ttl, yet later hops did. The router
			// is not dropping anything - it is configured not to originate ICMP
			// time-exceeded at all, which is common and completely benign.
			// Calling that "rate limiting" would misdescribe it.
			s.Insights = append(s.Insights, Insight{
				Kind: InsightSilent, TTL: h.TTL,
				Detail: "never answers, but hops beyond it do - this router does not send ICMP time-exceeded; it is not dropping traffic",
			})
		case !haveDownstream:
			// Last responding hop: the loss is the destination's own.
			s.Insights = append(s.Insights, Insight{
				Kind: InsightLossOnset, TTL: h.TTL, Addr: primary(h),
				Detail: fmt.Sprintf("%.1f%% loss at the destination (%d/%d probes)",
					h.Stats.LossPct, h.Stats.Lost, h.Stats.Sent),
			})
		case downstream+icmpLimitedSlackPct < h.Stats.LossPct:
			s.Insights = append(s.Insights, Insight{
				Kind: InsightICMPLimited, TTL: h.TTL, Addr: primary(h),
				Detail: fmt.Sprintf("%.1f%% loss here but %.1f%% at the cleanest hop downstream - this hop rate-limits its own ICMP replies, it is not dropping traffic",
					h.Stats.LossPct, downstream),
			})
		default:
			s.Insights = append(s.Insights, Insight{
				Kind: InsightLossOnset, TTL: h.TTL, Addr: primary(h),
				Detail: fmt.Sprintf("%.1f%% loss starts here and every hop downstream loses too (>=%.1f%%) - real forwarding loss",
					h.Stats.LossPct, downstream),
			})
		}
	}

	// Stalls: contiguous runs of rounds where a hop sat far above its own
	// baseline. Reported per hop, worst run first.
	for _, h := range s.Hops {
		base := h.Baseline()
		if base <= 0 || h.Stats.Recv < 5 {
			continue
		}
		limit := math.Max(base*stallFactor, base+stallFloorMS)
		var runStart, runEnd float64
		var runPeak float64
		var inRun bool
		flush := func() {
			if !inRun {
				return
			}
			inRun = false
			if runEnd-runStart < s.IntervalMS { // a single blip, not a stall
				return
			}
			s.Insights = append(s.Insights, Insight{
				Kind: InsightStall, TTL: h.TTL, Addr: primary(h),
				FromMS: runStart, ToMS: runEnd,
				Detail: fmt.Sprintf("stalled %s-%s, peak %.1fms vs %.1fms baseline (%.1fx)",
					fmtOffset(runStart), fmtOffset(runEnd), runPeak, base, runPeak/base),
			})
		}
		for _, sm := range h.Samples {
			if sm.Lost() || *sm.RTTMS <= limit {
				flush()
				continue
			}
			if !inRun {
				inRun, runStart, runPeak = true, sm.OffsetMS, 0
			}
			runEnd = sm.OffsetMS
			if *sm.RTTMS > runPeak {
				runPeak = *sm.RTTMS
			}
		}
		flush()
	}

	// When did loss happen? Fold the whole session into coarse buckets and
	// report the ones that carried a disproportionate share of the drops.
	s.Insights = append(s.Insights, s.lossWindows()...)

	// ECMP: more than one address answering at a TTL means the path moved.
	for _, h := range s.Hops {
		if len(h.Addrs) > 1 {
			s.Insights = append(s.Insights, Insight{
				Kind: InsightPathChange, TTL: h.TTL, Addr: primary(h),
				Detail: fmt.Sprintf("%d addresses answered at this hop (%s) - load balanced path",
					len(h.Addrs), strings.Join(h.Addrs, ", ")),
			})
		}
	}

	// Rank by how actionable each finding is, so the report leads with the
	// hop that is actually breaking things.
	order := map[string]int{
		InsightLossOnset: 0, InsightStall: 1, InsightLossWindow: 2,
		InsightICMPLimited: 3, InsightPathChange: 4, InsightSilent: 5,
	}
	sort.SliceStable(s.Insights, func(i, j int) bool {
		return order[s.Insights[i].Kind] < order[s.Insights[j].Kind]
	})

	if len(s.Insights) == 0 {
		s.Insights = []Insight{{Kind: InsightClean, Detail: "no loss and no latency stalls detected"}}
	}
}

// lossWindows finds the time ranges that concentrate loss. It buckets the
// session into at most 24 slices and reports slices whose loss rate is both
// meaningful in absolute terms and clearly worse than the session average.
func (s *Session) lossWindows() []Insight {
	const buckets = 24
	p := s.PlanCols(buckets, 0)

	type agg struct {
		sent, lost int
		from, to   float64
		worstTTL   int
		worstLoss  float64
	}
	cols := make([]agg, p.Cols)
	for i := range cols {
		cols[i].from = math.NaN()
	}
	var totalSent, totalLost int
	for _, h := range s.Hops {
		for _, c := range h.Cells(p) {
			if c.Empty() {
				continue
			}
			a := &cols[c.Col]
			if math.IsNaN(a.from) {
				a.from = c.FromMS
			}
			a.to = c.ToMS
			a.sent += c.Sent
			a.lost += c.Lost
			if c.LossPct > a.worstLoss {
				a.worstLoss, a.worstTTL = c.LossPct, h.TTL
			}
			totalSent += c.Sent
			totalLost += c.Lost
		}
	}
	if totalSent == 0 || totalLost == 0 {
		return nil
	}
	mean := float64(totalLost) / float64(totalSent) * 100

	hot := func(a agg) bool {
		if a.sent == 0 {
			return false
		}
		rate := float64(a.lost) / float64(a.sent) * 100
		return rate >= lossThresholdPct && rate >= mean*1.5
	}

	// Adjacent hot buckets are one event. Reporting each bucket separately turns
	// a single ten-second outage into ten near-identical lines.
	var out []Insight
	for i := 0; i < len(cols); {
		if !hot(cols[i]) {
			i++
			continue
		}
		merged := agg{from: cols[i].from}
		j := i
		for ; j < len(cols) && hot(cols[j]); j++ {
			merged.sent += cols[j].sent
			merged.lost += cols[j].lost
			merged.to = cols[j].to
			if cols[j].worstLoss > merged.worstLoss {
				merged.worstLoss, merged.worstTTL = cols[j].worstLoss, cols[j].worstTTL
			}
		}
		rate := float64(merged.lost) / float64(merged.sent) * 100
		out = append(out, Insight{
			Kind: InsightLossWindow, TTL: merged.worstTTL, FromMS: merged.from, ToMS: merged.to,
			Detail: fmt.Sprintf("%s to %s: %.1f%% of probes lost across the path (session average %.1f%%), worst at ttl %d",
				fmtOffset(merged.from), fmtOffset(merged.to), rate, mean, merged.worstTTL),
		})
		i = j
	}

	// Keep the report readable: the three worst windows carry the story.
	sort.SliceStable(out, func(i, j int) bool { return out[i].FromMS < out[j].FromMS })
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func primary(h *Hop) string {
	if len(h.Addrs) > 0 {
		return h.Addrs[0]
	}
	return ""
}

// fmtOffset renders a millisecond offset from session start. Short runs get
// tenths of a second: a five-second run reported as "+0:00 to +0:00" tells the
// reader nothing.
func fmtOffset(ms float64) string {
	sec := ms / 1000
	if sec < 60 {
		return fmt.Sprintf("+%.1fs", sec)
	}
	whole := int(sec)
	return fmt.Sprintf("+%d:%02d", whole/60, whole%60)
}
