package model

import (
	"math"
	"math/rand"
	"testing"
)

func fold(samples []Sample) *Accumulator {
	var a Accumulator
	for _, s := range samples {
		if s.Lost() {
			a.Add(0, true)
			continue
		}
		a.Add(*s.RTTMS, false)
	}
	return &a
}

// The whole point of the accumulator is that it can replace the samples. It has
// to agree with computing over them directly, or the long-term numbers and the
// window numbers would disagree about the same probes.
func TestAccumulatorAgreesWithComputingOverSamples(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	samples := make([]Sample, 0, 5000)
	for i := range 5000 {
		s := Sample{Round: i, OffsetMS: float64(i) * 250}
		if i%37 != 0 { // a little loss, so the jitter chain breaks too
			v := 10 + r.Float64()*90
			s.RTTMS = &v
		}
		samples = append(samples, s)
	}

	want := StatsOf(samples)
	got := fold(samples).Stats()

	if got.Sent != want.Sent || got.Recv != want.Recv || got.Lost != want.Lost {
		t.Errorf("sent/recv/lost = %d/%d/%d, want %d/%d/%d",
			got.Sent, got.Recv, got.Lost, want.Sent, want.Recv, want.Lost)
	}
	for _, c := range []struct {
		name       string
		got, want  float64
		tolerance  float64 // relative; percentiles come from a bucketed histogram
		exactMatch bool
	}{
		{name: "loss_pct", got: got.LossPct, want: want.LossPct, exactMatch: true},
		{name: "last", got: got.Last, want: want.Last, exactMatch: true},
		{name: "best", got: got.Best, want: want.Best, exactMatch: true},
		{name: "worst", got: got.Worst, want: want.Worst, exactMatch: true},
		{name: "avg", got: got.Avg, want: want.Avg, tolerance: 1e-9},
		{name: "stddev", got: got.StdDev, want: want.StdDev, tolerance: 1e-9},
		{name: "jitter", got: got.Jitter, want: want.Jitter, tolerance: 1e-9},
		{name: "p20", got: got.P20, want: want.P20, tolerance: 0.01},
		{name: "p50", got: got.P50, want: want.P50, tolerance: 0.01},
		{name: "p95", got: got.P95, want: want.P95, tolerance: 0.01},
	} {
		if c.exactMatch {
			if c.got != c.want {
				t.Errorf("%s = %v, want exactly %v", c.name, c.got, c.want)
			}
			continue
		}
		if rel := math.Abs(c.got-c.want) / c.want; rel > c.tolerance {
			t.Errorf("%s = %v, want %v (off by %.3f%%, tolerance %.3f%%)",
				c.name, c.got, c.want, rel*100, c.tolerance*100)
		}
	}
}

// Jitter is the mean change between consecutive replies, so a lost probe has to
// break the chain here exactly as it does over the samples.
func TestAccumulatorJitterBreaksOnLoss(t *testing.T) {
	var a Accumulator
	for _, v := range []float64{10, 20, -1, 100, 110} {
		if v < 0 {
			a.Add(0, true)
			continue
		}
		a.Add(v, false)
	}
	if j := a.Stats().Jitter; j != 10 {
		t.Errorf("jitter = %v, want 10 (the 20->100 jump spans a loss)", j)
	}
}

// A probe that arrives after its neighbours were folded still has to be counted.
// Only its jitter pair is skipped, because that is the one figure that depends
// on the order.
func TestAccumulatorAddLateCountsButDoesNotPair(t *testing.T) {
	var a Accumulator
	a.Add(10, false)
	a.Add(20, false)
	a.AddLate(1000, false)
	st := a.Stats()
	if st.Sent != 3 || st.Recv != 3 {
		t.Errorf("sent/recv = %d/%d, want 3/3: a late probe is still a probe", st.Sent, st.Recv)
	}
	if st.Worst != 1000 {
		t.Errorf("worst = %v, want 1000", st.Worst)
	}
	if st.Jitter != 10 {
		t.Errorf("jitter = %v, want 10 (the late probe must not be paired)", st.Jitter)
	}
}

// The histogram is the only lossy part of the long-term statistics, so its error
// has to stay under the precision the tool prints.
func TestHistogramQuantilesStayWithinDisplayPrecision(t *testing.T) {
	for _, v := range []float64{0.05, 0.5, 1, 9.9, 42, 137.5, 999, 2500} {
		var h hist
		for range 1000 {
			h.add(v)
		}
		got := h.quantile(50)
		if rel := math.Abs(got-v) / v; rel > 0.005 {
			t.Errorf("a constant %vms series reports p50 %v (off by %.2f%%)", v, got, rel*100)
		}
	}
}

func TestHistogramQuantilesAreOrdered(t *testing.T) {
	var h hist
	for i := range 1000 {
		h.add(float64(i) + 1)
	}
	p20, p50, p95 := h.quantile(20), h.quantile(50), h.quantile(95)
	if !(p20 < p50 && p50 < p95) {
		t.Errorf("quantiles out of order: p20=%v p50=%v p95=%v", p20, p50, p95)
	}
	if math.Abs(p50-500) > 500*0.01 {
		t.Errorf("p50 of 1..1000 = %v, want ~500", p50)
	}
}

// CopyInto is on the redraw path: it must be a real copy, or a frame's
// provisional probes would leak into the committed statistics.
func TestCopyIntoDoesNotShareHistogramStorage(t *testing.T) {
	var a Accumulator
	for range 100 {
		a.Add(10, false)
	}
	var b Accumulator
	a.CopyInto(&b)
	b.Add(5000, false)

	if got := a.Stats().Worst; got != 10 {
		t.Errorf("the original saw the copy's probe: worst = %v, want 10", got)
	}
	if got := a.Stats().P95; math.Abs(got-10) > 0.1 {
		t.Errorf("the original's percentiles moved: p95 = %v, want ~10", got)
	}
	if got := b.Stats().Worst; got != 5000 {
		t.Errorf("the copy did not take the probe: worst = %v, want 5000", got)
	}
}
