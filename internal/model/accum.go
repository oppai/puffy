package model

import "math"

// Accumulator folds a hop's probe outcomes into statistics without keeping the
// probes. It is what makes the long-term numbers honest on a run that has to
// trim its raw time series to stay bounded: a session that dropped its oldest
// samples still knows exactly what it measured.
//
// Add must be called in round order, because jitter is defined over
// consecutive replies and nothing else here cares about order. AddLate exists
// for the probe that arrives after its neighbours have already been folded.
type Accumulator struct {
	sent, recv, lost int
	last             float64
	best, worst      float64
	mean, m2         float64 // Welford: an hours-long run must not lose precision
	jitterSum        float64
	jitterN          int
	prev             float64
	havePrev         bool
	hist             hist
}

// Add folds one probe. rtt is in milliseconds and is ignored when lost.
func (a *Accumulator) Add(rtt float64, lost bool) { a.add(rtt, lost, true) }

// AddLate folds a probe that arrived out of round order. Counts, extremes and
// percentiles do not depend on order, so only the consecutive-reply chain that
// jitter is built from is skipped rather than mispaired.
func (a *Accumulator) AddLate(rtt float64, lost bool) { a.add(rtt, lost, false) }

func (a *Accumulator) add(rtt float64, lost, ordered bool) {
	a.sent++
	if lost {
		a.lost++
		if ordered {
			a.havePrev = false // a gap breaks the consecutive-pair chain
		}
		return
	}
	a.recv++
	a.last = rtt
	if a.recv == 1 || rtt < a.best {
		a.best = rtt
	}
	if rtt > a.worst {
		a.worst = rtt
	}
	d := rtt - a.mean
	a.mean += d / float64(a.recv)
	a.m2 += d * (rtt - a.mean)
	a.hist.add(rtt)

	if !ordered {
		return
	}
	if a.havePrev {
		a.jitterSum += math.Abs(rtt - a.prev)
		a.jitterN++
	}
	a.prev, a.havePrev = rtt, true
}

// Sent is how many probes have been folded so far.
func (a *Accumulator) Sent() int { return a.sent }

// CopyInto overwrites dst with a copy of a, reusing dst's histogram storage.
// The live views need the statistics as they stand plus a handful of probes too
// recent to commit, and a redraw four times a second should not allocate a
// histogram to find that out.
func (a *Accumulator) CopyInto(dst *Accumulator) {
	counts := dst.hist.counts
	*dst = *a
	if a.hist.counts == nil {
		dst.hist.counts = nil
		return
	}
	if cap(counts) < len(a.hist.counts) {
		counts = make([]uint32, len(a.hist.counts))
	}
	counts = counts[:len(a.hist.counts)]
	copy(counts, a.hist.counts)
	dst.hist.counts = counts
}

// Stats renders the long-term summary. Percentiles come from a log-scale
// histogram, so they are accurate to half a percent - finer than anything puffy
// prints - at fixed memory.
func (a *Accumulator) Stats() Stats {
	st := Stats{Sent: a.sent, Recv: a.recv, Lost: a.lost, Last: a.last}
	if a.sent > 0 {
		st.LossPct = float64(a.lost) / float64(a.sent) * 100
	}
	if a.recv == 0 {
		return st
	}
	st.Best, st.Worst, st.Avg = a.best, a.worst, a.mean
	st.StdDev = math.Sqrt(a.m2 / float64(a.recv))
	if a.jitterN > 0 {
		st.Jitter = a.jitterSum / float64(a.jitterN)
	}
	st.P20 = a.hist.quantile(20)
	st.P50 = a.hist.quantile(50)
	st.P95 = a.hist.quantile(95)
	return st
}

// hist is a log-scale histogram of round-trip times. Percentiles and the hop
// baseline have to describe the whole session, not the samples that happen to
// still be in memory, and sorting a million RTTs on every redraw is exactly the
// cost this tool cannot afford - so the distribution is kept, not the values.
//
// Buckets grow by half a percent, which is finer than the tool's own display
// precision, and the range reaches from a loopback reply to well past any
// usable timeout.
const (
	histBase    = 1.005
	histMin     = 0.005 // ms; anything faster lands in the first bucket
	histBuckets = 3400  // top of the range is ~115s
)

type hist struct {
	counts []uint32
	n      int
	lo, hi int // the bucket range actually used, so a quantile scan stays short
}

func (h *hist) add(v float64) {
	if h.counts == nil {
		h.counts = make([]uint32, histBuckets)
		h.lo, h.hi = histBuckets-1, 0
	}
	i := histBucket(v)
	h.counts[i]++
	h.n++
	if i < h.lo {
		h.lo = i
	}
	if i > h.hi {
		h.hi = i
	}
}

func histBucket(v float64) int {
	if v <= histMin {
		return 0
	}
	i := int(math.Log(v/histMin) / math.Log(histBase))
	if i >= histBuckets {
		return histBuckets - 1
	}
	return i
}

// quantile is the nearest-rank quantile, reported at the geometric centre of
// the bucket it lands in - the same rule for every value, so two percentiles of
// one distribution can never cross.
func (h *hist) quantile(p float64) float64 {
	if h.n == 0 {
		return 0
	}
	want := int(math.Ceil(p / 100 * float64(h.n)))
	if want < 1 {
		want = 1
	}
	cum := 0
	for i := h.lo; i <= h.hi; i++ {
		cum += int(h.counts[i])
		if cum >= want {
			return histMin * math.Pow(histBase, float64(i)+0.5)
		}
	}
	return histMin * math.Pow(histBase, float64(h.hi)+0.5)
}
