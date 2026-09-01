// Package model holds puffy's on-the-wire measurement schema. Everything the
// terminal UI, the JSON writer and the HTML report render is derived from a
// Session, so the three views can never disagree about what was measured.
package model

import (
	"math"
	"sort"
	"time"
)

// Session is one puffy run. It is the root of the JSON document.
type Session struct {
	Tool       string    `json:"tool"`
	Version    string    `json:"version"`
	Schema     int       `json:"schema"`
	Mode       string    `json:"mode"` // "ping" or "trace"
	Target     string    `json:"target"`
	TargetIP   string    `json:"target_ip"`
	TargetName string    `json:"target_name,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	IntervalMS float64   `json:"interval_ms"`
	TimeoutMS  float64   `json:"timeout_ms"`
	Rounds     int       `json:"rounds"`
	// SamplesFrom is the first round that still carries raw samples. A long run
	// trims its oldest probes to stay bounded, so Rounds counts what was
	// measured while this says how far back the time series itself reaches. The
	// per-hop Stats stay exact either way: they are folded as probes arrive, not
	// derived from the samples that happen to still be here.
	SamplesFrom int       `json:"samples_from_round,omitempty"`
	Hops        []*Hop    `json:"hops"`
	Insights    []Insight `json:"insights,omitempty"`
}

// Hop is one TTL position on the path. In ping mode a Session has exactly one.
type Hop struct {
	TTL     int      `json:"ttl"`
	Addrs   []string `json:"addrs"` // more than one when ECMP spreads the path
	Name    string   `json:"name,omitempty"`
	Final   bool     `json:"final"` // this hop answered as the destination
	Samples []Sample `json:"samples"`
	Stats   Stats    `json:"stats"`
}

// Sample is a single probe outcome. RTTMS is nil when the probe was lost, which
// is what makes loss a first-class value in the time series rather than a gap.
type Sample struct {
	Round    int      `json:"r"`
	OffsetMS float64  `json:"t"` // ms since Session.StartedAt
	RTTMS    *float64 `json:"rtt"`
	Addr     string   `json:"ip,omitempty"`
}

// Lost reports whether the probe never came back.
func (s Sample) Lost() bool { return s.RTTMS == nil }

// Stats are the scalar summaries shown in the hop table and the report tiles.
type Stats struct {
	Sent    int     `json:"sent"`
	Recv    int     `json:"recv"`
	Lost    int     `json:"lost"`
	LossPct float64 `json:"loss_pct"`
	Last    float64 `json:"last_ms"`
	Best    float64 `json:"best_ms"`
	Avg     float64 `json:"avg_ms"`
	Worst   float64 `json:"worst_ms"`
	P20     float64 `json:"p20_ms"` // the uncongested reference level
	P50     float64 `json:"p50_ms"`
	P95     float64 `json:"p95_ms"`
	StdDev  float64 `json:"stddev_ms"`
	Jitter  float64 `json:"jitter_ms"` // mean |RTT(n) - RTT(n-1)|, consecutive replies only
}

// Insight is a machine-derived reading of the path: the answers to "where does
// loss concentrate" and "when did it stall" that the graphs show visually.
type Insight struct {
	Kind   string  `json:"kind"`
	TTL    int     `json:"ttl,omitempty"`
	Addr   string  `json:"addr,omitempty"`
	FromMS float64 `json:"from_ms,omitempty"`
	ToMS   float64 `json:"to_ms,omitempty"`
	Detail string  `json:"detail"`
}

// Insight kinds.
const (
	InsightLossOnset   = "loss_onset"   // real loss starts here and persists downstream
	InsightICMPLimited = "icmp_limited" // loss at this hop only: rate-limited replies, not a path fault
	InsightSilent      = "silent"       // this ttl never answers at all, but traffic passes through it
	InsightStall       = "stall"        // RTT inflated far above this hop's own baseline
	InsightLossWindow  = "loss_window"  // a time range where loss concentrated
	InsightPathChange  = "path_change"  // several addresses answered at one TTL
	InsightClean       = "clean"        // nothing worth reporting
)

// Dest returns the last hop that answered, which is the destination in a
// completed trace and the only hop in a ping.
func (s *Session) Dest() *Hop {
	for i := len(s.Hops) - 1; i >= 0; i-- {
		if s.Hops[i].Stats.Recv > 0 {
			return s.Hops[i]
		}
	}
	if len(s.Hops) > 0 {
		return s.Hops[len(s.Hops)-1]
	}
	return nil
}

// Label renders the hop's identity for a table row: its name when resolved, its
// address otherwise, and "*" when nothing ever answered.
func (h *Hop) Label() string {
	if h.Name != "" {
		return h.Name
	}
	if len(h.Addrs) > 0 {
		if len(h.Addrs) > 1 {
			return h.Addrs[0] + " +" + itoa(len(h.Addrs)-1)
		}
		return h.Addrs[0]
	}
	return "*"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Recompute refreshes the derived Stats of every hop from its samples.
//
// Statistics that already cover more probes than the samples do are left alone.
// A long run trims its oldest samples, so recomputing there would quietly
// replace "what this hop did all day" with "what it did in the part we kept" -
// and the same document would then say two different things depending on whether
// it was read by the run that made it or by a later render.
func (s *Session) Recompute() {
	for _, h := range s.Hops {
		st := computeStats(h.Samples)
		if s.SamplesFrom > 0 && h.Stats.Sent > st.Sent {
			continue
		}
		h.Stats = st
	}
}

func computeStats(samples []Sample) Stats {
	st := Stats{Best: math.NaN()}
	var sum float64
	rtts := make([]float64, 0, len(samples))
	var prev float64
	var havePrev bool
	var jitterSum float64
	var jitterN int

	for _, sm := range samples {
		st.Sent++
		if sm.Lost() {
			st.Lost++
			havePrev = false // a gap breaks the consecutive-pair chain
			continue
		}
		v := *sm.RTTMS
		st.Recv++
		sum += v
		rtts = append(rtts, v)
		st.Last = v
		if math.IsNaN(st.Best) || v < st.Best {
			st.Best = v
		}
		if v > st.Worst {
			st.Worst = v
		}
		if havePrev {
			jitterSum += math.Abs(v - prev)
			jitterN++
		}
		prev, havePrev = v, true
	}

	if st.Sent > 0 {
		st.LossPct = float64(st.Lost) / float64(st.Sent) * 100
	}
	if math.IsNaN(st.Best) {
		st.Best = 0
	}
	if st.Recv == 0 {
		return st
	}

	st.Avg = sum / float64(st.Recv)
	for _, v := range rtts {
		d := v - st.Avg
		st.StdDev += d * d
	}
	st.StdDev = math.Sqrt(st.StdDev / float64(st.Recv))
	if jitterN > 0 {
		st.Jitter = jitterSum / float64(jitterN)
	}
	sorted := append([]float64(nil), rtts...)
	sort.Float64s(sorted)
	st.P20 = percentile(sorted, 20)
	st.P50 = percentile(sorted, 50)
	st.P95 = percentile(sorted, 95)
	return st
}

// StatsOf summarises an arbitrary slice of samples. The live views use it for
// the window they draw, so the short-term numbers describe exactly the probes
// on screen and nothing else - the long-term numbers in Hop.Stats cover the
// whole run, including probes whose samples have since been trimmed.
func StatsOf(samples []Sample) Stats { return computeStats(samples) }

// percentile is the nearest-rank percentile of an already-sorted slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// Baseline is the hop's uncongested reference latency: the 20th percentile of
// its replies. Comparing a sample against this - rather than against the
// session-wide scale - is what separates "this hop is slow" from "this hop is
// stalling right now".
//
// It is deliberately a whole-session figure. A baseline recomputed from the
// visible window would drift up while the hop is stalling, and the stall would
// then colour itself normal a minute after it started.
func (h *Hop) Baseline() float64 {
	if h.Stats.Recv > 0 {
		return h.Stats.P20
	}
	return baselineOf(h.Samples)
}

func baselineOf(samples []Sample) float64 {
	rtts := make([]float64, 0, len(samples))
	for _, s := range samples {
		if !s.Lost() {
			rtts = append(rtts, *s.RTTMS)
		}
	}
	if len(rtts) == 0 {
		return 0
	}
	sort.Float64s(rtts)
	return percentile(rtts, 20)
}

// RetainedRounds is how many trailing rounds still have samples to draw. The
// graphs plan their columns over this rather than over Rounds, so a run whose
// oldest probes have been trimmed does not reserve columns for them.
func (s *Session) RetainedRounds() int {
	n := s.Rounds - s.SamplesFrom
	if n < 1 {
		return 1
	}
	return n
}

// Elapsed is how long the session has been running, in milliseconds. It is the
// span the long-term statistics cover.
func (s *Session) Elapsed() float64 {
	d := s.EndedAt.Sub(s.StartedAt)
	if d < 0 {
		return 0
	}
	return float64(d) / float64(time.Millisecond)
}

// MaxRTT is the largest RTT seen anywhere in the session, used to put every hop
// on one shared vertical scale.
func (s *Session) MaxRTT() float64 {
	var m float64
	for _, h := range s.Hops {
		if h.Stats.Worst > m {
			m = h.Stats.Worst
		}
	}
	return m
}
