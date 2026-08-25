package model

import (
	"strings"
	"testing"
	"time"
)

// hop builds a hop whose samples follow a pattern string: '.' is a reply at the
// given rtt, 'x' is a lost probe, 'S' is a reply at four times the rtt.
func hop(ttl int, addr string, rtt float64, pattern string) *Hop {
	h := &Hop{TTL: ttl, Addrs: []string{addr}}
	for i, ch := range pattern {
		s := Sample{Round: i, OffsetMS: float64(i) * 1000}
		switch ch {
		case 'x':
			// lost: RTTMS stays nil
		case 'S':
			v := rtt * 4
			s.RTTMS = &v
			s.Addr = addr
		default:
			v := rtt
			s.RTTMS = &v
			s.Addr = addr
		}
		h.Samples = append(h.Samples, s)
	}
	return h
}

func session(hops ...*Hop) *Session {
	rounds := 0
	for _, h := range hops {
		if len(h.Samples) > rounds {
			rounds = len(h.Samples)
		}
	}
	return &Session{
		Tool: "puffy", Mode: "trace", Target: "test", TargetIP: "192.0.2.1",
		StartedAt: time.Unix(0, 0), EndedAt: time.Unix(int64(rounds), 0),
		IntervalMS: 1000, TimeoutMS: 2000, Rounds: rounds, Hops: hops,
	}
}

func kinds(s *Session) map[string][]Insight {
	out := map[string][]Insight{}
	for _, in := range s.Insights {
		out[in.Kind] = append(out[in.Kind], in)
	}
	return out
}

// A router that drops only its own replies must not be reported as a fault: the
// giveaway is that hops behind it still answer cleanly.
func TestAnalyzeICMPRateLimitNotReportedAsLoss(t *testing.T) {
	s := session(
		hop(1, "10.0.0.1", 2, "..xx..xx..xx..xx..xx"),
		hop(2, "10.0.0.2", 5, "...................."),
		hop(3, "10.0.0.3", 9, "...................."),
	)
	s.Analyze()
	got := kinds(s)

	if len(got[InsightICMPLimited]) != 1 {
		t.Fatalf("want one icmp_limited insight, got %d (%+v)", len(got[InsightICMPLimited]), s.Insights)
	}
	if ttl := got[InsightICMPLimited][0].TTL; ttl != 1 {
		t.Errorf("icmp_limited attributed to ttl %d, want 1", ttl)
	}
	if n := len(got[InsightLossOnset]); n != 0 {
		t.Errorf("reported %d loss_onset insights for a rate-limiting hop: %+v", n, got[InsightLossOnset])
	}
}

// Two consecutive rate-limiting routers must not cover for each other. This is
// why the comparison is against the least lossy hop downstream, not the worst.
func TestAnalyzeConsecutiveRateLimitersBothExonerated(t *testing.T) {
	s := session(
		hop(1, "10.0.0.1", 2, "..xx..xx..xx..xx..xx"),
		hop(2, "10.0.0.2", 5, "..xx..xx..xx..xx..xx"),
		hop(3, "10.0.0.3", 9, "...................."),
		hop(4, "10.0.0.4", 11, "...................."),
	)
	s.Analyze()
	got := kinds(s)

	if n := len(got[InsightICMPLimited]); n != 2 {
		t.Errorf("want both hops flagged as rate limiting, got %d: %+v", n, s.Insights)
	}
	if n := len(got[InsightLossOnset]); n != 0 {
		t.Errorf("blamed a rate-limiting hop for forwarding loss: %+v", got[InsightLossOnset])
	}
}

// Loss that persists to the end of the path is real, and must be attributed to
// the first hop where it appears.
func TestAnalyzeRealLossAttributedToOnsetHop(t *testing.T) {
	s := session(
		hop(1, "10.0.0.1", 2, "...................."),
		hop(2, "10.0.0.2", 5, "...................."),
		hop(3, "10.0.0.3", 9, "..xx..xx..xx..xx..xx"),
		hop(4, "10.0.0.4", 11, "..xx..xx..xx..xx..xx"),
	)
	s.Analyze()
	got := kinds(s)

	if len(got[InsightLossOnset]) == 0 {
		t.Fatalf("real forwarding loss was not reported: %+v", s.Insights)
	}
	if ttl := got[InsightLossOnset][0].TTL; ttl != 3 {
		t.Errorf("loss onset attributed to ttl %d, want 3 (the first hop that loses)", ttl)
	}
	if n := len(got[InsightICMPLimited]); n != 0 {
		t.Errorf("real loss was excused as rate limiting: %+v", got[InsightICMPLimited])
	}
}

func TestAnalyzeCleanPathSaysSo(t *testing.T) {
	s := session(
		hop(1, "10.0.0.1", 2, ".........."),
		hop(2, "10.0.0.2", 5, ".........."),
	)
	s.Analyze()
	if len(s.Insights) != 1 || s.Insights[0].Kind != InsightClean {
		t.Errorf("clean path produced %+v", s.Insights)
	}
}

func TestAnalyzeStallReportsPeakAgainstBaseline(t *testing.T) {
	s := session(hop(1, "10.0.0.1", 10, "......SSSS......"))
	s.Analyze()
	got := kinds(s)
	if len(got[InsightStall]) != 1 {
		t.Fatalf("want one stall insight, got %+v", s.Insights)
	}
	if d := got[InsightStall][0].Detail; !strings.Contains(d, "4.0x") {
		t.Errorf("stall detail does not name the inflation factor: %q", d)
	}
}

// Adjacent bad time slices are one event. Reporting each separately turns a
// single outage into a wall of near-identical lines.
func TestAnalyzeMergesAdjacentLossWindows(t *testing.T) {
	// One contiguous outage in the middle of an otherwise clean run.
	s := session(
		hop(1, "10.0.0.1", 2, strings.Repeat(".", 20)+strings.Repeat("x", 10)+strings.Repeat(".", 20)),
		hop(2, "10.0.0.2", 5, strings.Repeat(".", 20)+strings.Repeat("x", 10)+strings.Repeat(".", 20)),
	)
	s.Analyze()
	if n := len(kinds(s)[InsightLossWindow]); n != 1 {
		t.Errorf("one contiguous outage produced %d loss_window insights: %+v", n, s.Insights)
	}
}

func TestStatsLossAndPercentiles(t *testing.T) {
	h := hop(1, "10.0.0.1", 10, "..x.")
	st := computeStats(h.Samples)
	if st.Sent != 4 || st.Recv != 3 || st.Lost != 1 {
		t.Errorf("sent/recv/lost = %d/%d/%d, want 4/3/1", st.Sent, st.Recv, st.Lost)
	}
	if st.LossPct != 25 {
		t.Errorf("loss = %v%%, want 25%%", st.LossPct)
	}
	if st.Avg != 10 || st.Best != 10 || st.Worst != 10 {
		t.Errorf("avg/best/worst = %v/%v/%v, want 10/10/10", st.Avg, st.Best, st.Worst)
	}
}

// Jitter is the mean change between *consecutive replies*. A lost probe breaks
// the chain rather than pairing the samples either side of the gap.
func TestStatsJitterIgnoresPairsAcrossLoss(t *testing.T) {
	h := &Hop{TTL: 1}
	for i, v := range []float64{10, 20, -1, 100, 110} {
		s := Sample{Round: i, OffsetMS: float64(i) * 1000}
		if v > 0 {
			val := v
			s.RTTMS = &val
		}
		h.Samples = append(h.Samples, s)
	}
	st := computeStats(h.Samples)
	// Pairs are (10,20) and (100,110): both differ by 10. The 20->100 jump
	// spans the lost probe and must not count.
	if st.Jitter != 10 {
		t.Errorf("jitter = %v, want 10 (pairs across a loss must be skipped)", st.Jitter)
	}
}

func TestPlanColsFoldsHistoryIntoWidth(t *testing.T) {
	s := session(hop(1, "10.0.0.1", 5, strings.Repeat(".", 100)))
	p := s.PlanCols(25, 0)
	if p.PerCol != 4 {
		t.Errorf("100 rounds into 25 columns gave PerCol=%d, want 4", p.PerCol)
	}
	if p.Cols != 25 {
		t.Errorf("Cols=%d, want 25", p.Cols)
	}
	// Every round must land in a column, and none outside the grid.
	for r := p.FirstRound; r <= p.LastRound; r++ {
		if c := p.Col(r); c < 0 || c >= p.Cols {
			t.Fatalf("round %d mapped to column %d, outside 0..%d", r, c, p.Cols-1)
		}
	}
}

func TestCellsAggregateLossAndWorstRTT(t *testing.T) {
	// Four rounds per column: the first column holds two replies and two losses.
	s := session(hop(1, "10.0.0.1", 10, "..xx"+"...."))
	s.Hops[0].Samples[1].RTTMS = ptr(40) // make the worst distinguishable
	p := s.PlanCols(2, 0)
	cells := s.Hops[0].Cells(p)
	if len(cells) != 2 {
		t.Fatalf("want 2 cells, got %d", len(cells))
	}
	if cells[0].Sent != 4 || cells[0].Lost != 2 || cells[0].LossPct != 50 {
		t.Errorf("cell 0 = %+v, want sent 4 lost 2 loss 50%%", cells[0])
	}
	if cells[0].MaxRTT != 40 {
		t.Errorf("cell 0 MaxRTT = %v, want 40 (the worst in the bucket)", cells[0].MaxRTT)
	}
	if cells[0].AvgRTT != 25 {
		t.Errorf("cell 0 AvgRTT = %v, want 25", cells[0].AvgRTT)
	}
}

func TestBaselineIsLowPercentileNotMinimum(t *testing.T) {
	h := &Hop{TTL: 1}
	for i, v := range []float64{10, 10, 10, 10, 10, 200, 200, 200, 200, 200} {
		val := v
		h.Samples = append(h.Samples, Sample{Round: i, RTTMS: &val})
	}
	if b := h.Baseline(); b != 10 {
		t.Errorf("baseline = %v, want 10 (the uncongested level, not the mean)", b)
	}
}

func ptr(v float64) *float64 { return &v }
