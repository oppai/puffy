package probe

import (
	"context"
	"math"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oppai/puffy/internal/model"
)

// Version is stamped into every session document. It is a var, not a const, so
// a build can overwrite it with the actual tag:
//
//	go build -ldflags "-X github.com/oppai/puffy/internal/probe.Version=$(git describe --tags)"
//
// A plain `go install` keeps the value below, which is what an untagged source
// build should report.
var Version = "0.1.0"

// schemaVersion lets a future puffy tell whether it can read an old JSON file.
const schemaVersion = 1

// reading is one probe outcome as the collector stores it: no pointers, so a
// day-long session is a few flat arrays the garbage collector never has to walk
// rather than millions of separately allocated floats.
type reading struct {
	round int
	offMS float64
	rtt   float64 // milliseconds; meaningless when lost
	lost  bool
	addr  uint8 // 1-based index into hopState.addrs, 0 when nothing answered
}

// hopState is what the collector keeps for one TTL: a rolling window of raw
// probes for the graphs, and an accumulator that has already folded every probe
// ever taken. The split is the whole point - the window is what you can draw,
// the accumulator is what you measured.
type hopState struct {
	ttl   int
	final bool
	addrs []string

	readings []reading // round-ordered, trailing history only
	folded   int       // index of the first reading not yet in acc

	acc  model.Accumulator
	prov model.Accumulator // scratch: acc plus the not-yet-foldable tail
}

// Collector folds the engine's out-of-order result stream into a Session. It is
// safe for the renderer to read a snapshot while results keep arriving.
type Collector struct {
	mu      sync.Mutex
	session model.Session
	hops    map[int]*hopState
	// finalTTL is the shortest TTL that reached the destination. Hops past it
	// are dropped from the view: they are TTLs we probed before we knew where
	// the path ended, not real hops.
	finalTTL int
	maxRound int

	// history is how many trailing rounds of raw samples to keep, and lag is how
	// many rounds a probe can still be outstanding for.
	history int
	lag     int

	resolve  bool
	names    map[netip.Addr]string
	resolved map[netip.Addr]bool
}

// NewCollector starts a session document for the given configuration.
func NewCollector(cfg Config, resolveNames bool) *Collector {
	// A probe is resolved within one timeout of being sent, so a reading whose
	// round is more than this many rounds behind the newest one can no longer be
	// preceded by a late arrival. That is what lets the accumulator fold in
	// round order, which is what jitter needs.
	lag := 2
	if cfg.Interval > 0 {
		lag += int(cfg.Timeout / cfg.Interval)
	}
	lag = min(max(lag, 2), 4096)

	history := cfg.History
	if history < 0 {
		history = 0
	}
	if history > 0 && history < 2*lag {
		// Readings cannot be dropped before they have been counted, and a probe
		// is only counted once no earlier round can still arrive. Asking for less
		// history than that would not be honoured, so it is raised here rather
		// than quietly ignored.
		history = 2 * lag
	}

	return &Collector{
		session: model.Session{
			Tool:       "puffy",
			Version:    Version,
			Schema:     schemaVersion,
			Mode:       cfg.Mode,
			Target:     cfg.Target,
			TargetIP:   cfg.IP.String(),
			StartedAt:  time.Now(),
			IntervalMS: float64(cfg.Interval) / float64(time.Millisecond),
			TimeoutMS:  float64(cfg.Timeout) / float64(time.Millisecond),
		},
		hops:     make(map[int]*hopState),
		history:  history,
		lag:      lag,
		resolve:  resolveNames,
		names:    make(map[netip.Addr]string),
		resolved: make(map[netip.Addr]bool),
	}
}

// StartedAt is the session's reference time for sample offsets.
func (c *Collector) StartedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session.StartedAt
}

// SetTargetName records the forward-resolved name of the target.
func (c *Collector) SetTargetName(name string) {
	c.mu.Lock()
	c.session.TargetName = name
	c.mu.Unlock()
}

// Add folds one probe result into the session.
func (c *Collector) Add(r Result) {
	c.mu.Lock()

	hs, ok := c.hops[r.TTL]
	if !ok {
		hs = &hopState{ttl: r.TTL}
		c.hops[r.TTL] = hs
	}

	rd := reading{
		round: r.Round,
		offMS: float64(r.Sent.Sub(c.session.StartedAt)) / float64(time.Millisecond),
		lost:  true,
	}
	// An unreachable reply proves the hop is on the path and tells us how long
	// the round trip took, so it is a measurement, not a loss. The code is kept
	// in the insight layer rather than the sample.
	if r.Kind.Replied() {
		rd.lost = false
		rd.rtt = float64(r.RTT) / float64(time.Millisecond)
		if r.From.IsValid() {
			rd.addr = hs.noteAddr(r.From)
		}
	}
	hs.insert(rd)

	if r.Kind.Reached() {
		hs.final = true
		if c.finalTTL == 0 || r.TTL < c.finalTTL {
			c.finalTTL = r.TTL
		}
	}
	if r.Round > c.maxRound {
		c.maxRound = r.Round
	}
	hs.fold(c.maxRound - c.lag)
	hs.trim(c.history)

	needLookup := c.resolve && r.From.IsValid() && !c.resolved[r.From]
	if needLookup {
		c.resolved[r.From] = true
	}
	c.mu.Unlock()

	if needLookup {
		go c.lookup(r.From)
	}
}

// noteAddr records an address seen at this hop and returns its 1-based index.
// Several addresses at one TTL mean the path is load balanced, which is worth
// surfacing rather than hiding behind whichever router answered last.
func (hs *hopState) noteAddr(addr netip.Addr) uint8 {
	s := addr.String()
	for i, existing := range hs.addrs {
		if existing == s {
			return uint8(i + 1)
		}
	}
	if len(hs.addrs) >= 255 {
		// An ECMP fan-out this wide is already reported as a path change; the
		// index is one byte because that is what keeps a reading pointer-free.
		return 0
	}
	hs.addrs = append(hs.addrs, s)
	return uint8(len(hs.addrs))
}

// insert appends in round order. Results arrive out of order - a fast hop in a
// later round can beat a slow one, and a timeout only lands a whole timeout
// after it was sent - and jitter plus the trailing-window views assume the
// slice is ordered, so a short backward scan pays for itself.
func (hs *hopState) insert(rd reading) {
	i := len(hs.readings)
	for i > 0 && hs.readings[i-1].round > rd.round {
		i--
	}
	if i == len(hs.readings) {
		hs.readings = append(hs.readings, rd)
	} else {
		hs.readings = append(hs.readings, reading{})
		copy(hs.readings[i+1:], hs.readings[i:])
		hs.readings[i] = rd
	}
	if i < hs.folded {
		// Later than the fold cursor thought possible. It still has to be
		// counted, so it is folded out of order and the cursor keeps pointing at
		// the same unfolded reading.
		hs.acc.AddLate(rd.rtt, rd.lost)
		hs.folded++
	}
}

// fold moves readings up to and including round limit into the long-term
// accumulator. Only rounds no late arrival can precede are folded, so the
// accumulator sees every probe exactly once and in order.
func (hs *hopState) fold(limit int) {
	for hs.folded < len(hs.readings) && hs.readings[hs.folded].round <= limit {
		rd := hs.readings[hs.folded]
		hs.acc.Add(rd.rtt, rd.lost)
		hs.folded++
	}
}

// trim drops the oldest readings once the raw history is over budget, in chunks
// so the copy is amortised, and never past the fold cursor - nothing is dropped
// before it has been counted.
func (hs *hopState) trim(keep int) {
	if keep <= 0 || len(hs.readings) <= keep+keep/4 {
		return
	}
	drop := min(len(hs.readings)-keep, hs.folded)
	if drop <= 0 {
		return
	}
	hs.readings = hs.readings[:copy(hs.readings, hs.readings[drop:])]
	hs.folded -= drop
}

// stats is the hop's long-term summary, including the recent probes that are
// still too new to fold. The accumulator itself is left alone: those probes may
// yet be joined by a late arrival from an earlier round.
func (hs *hopState) stats() model.Stats {
	if hs.folded == len(hs.readings) {
		return hs.acc.Stats()
	}
	hs.acc.CopyInto(&hs.prov)
	for _, rd := range hs.readings[hs.folded:] {
		hs.prov.Add(rd.rtt, rd.lost)
	}
	return hs.prov.Stats()
}

// samples materialises the readings from fromRound onwards as schema samples.
// The RTTs share one backing array, so a frame costs a handful of allocations
// rather than one per probe.
func (hs *hopState) samples(fromRound int) []model.Sample {
	i := sort.Search(len(hs.readings), func(i int) bool { return hs.readings[i].round >= fromRound })
	tail := hs.readings[i:]
	if len(tail) == 0 {
		return nil
	}
	rtts := make([]float64, len(tail))
	out := make([]model.Sample, len(tail))
	for j, rd := range tail {
		out[j] = model.Sample{Round: rd.round, OffsetMS: rd.offMS}
		if !rd.lost {
			rtts[j] = rd.rtt
			out[j].RTTMS = &rtts[j]
		}
		if rd.addr > 0 {
			out[j].Addr = hs.addrs[rd.addr-1]
		}
	}
	return out
}

func (c *Collector) lookup(addr netip.Addr) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(ctx, addr.String())
	if err != nil || len(names) == 0 {
		return
	}
	c.mu.Lock()
	c.names[addr] = strings.TrimSuffix(names[0], ".")
	c.mu.Unlock()
}

// Snapshot returns a consistent copy of the session for rendering or writing.
// Only the trailing window rounds of raw samples are materialised - 0 means
// everything still held - which is what keeps a redraw costing the same at
// hour six as at second one. Hops past the destination are omitted, hop names
// are filled in from the reverse-DNS cache, and the per-hop statistics cover
// the whole run whatever the window is.
func (c *Collector) Snapshot(window int) *model.Session {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.session
	s.Rounds = c.maxRound + 1
	s.EndedAt = time.Now()
	if !c.session.EndedAt.IsZero() {
		s.EndedAt = c.session.EndedAt
	}

	from := 0
	if window > 0 {
		from = max(c.maxRound-window+1, 0)
	}

	ttls := make([]int, 0, len(c.hops))
	for ttl := range c.hops {
		if c.finalTTL > 0 && ttl > c.finalTTL {
			continue
		}
		ttls = append(ttls, ttl)
	}
	sort.Ints(ttls)

	first := math.MaxInt
	s.Hops = make([]*model.Hop, 0, len(ttls))
	for _, ttl := range ttls {
		hs := c.hops[ttl]
		h := &model.Hop{
			TTL:     hs.ttl,
			Final:   hs.final,
			Addrs:   append([]string(nil), hs.addrs...),
			Samples: hs.samples(from),
			Stats:   hs.stats(),
		}
		if len(h.Addrs) > 0 {
			if addr, err := netip.ParseAddr(h.Addrs[0]); err == nil {
				h.Name = c.names[addr]
			}
		}
		if len(h.Samples) > 0 {
			first = min(first, h.Samples[0].Round)
		}
		s.Hops = append(s.Hops, h)
	}
	if first != math.MaxInt {
		s.SamplesFrom = first
	}
	return &s
}

// Finish stamps the end time, folds every outstanding probe into the long-term
// statistics and runs the analysis pass. Every sample still held is included:
// the session document is the whole retained history, not the last screenful.
func (c *Collector) Finish() *model.Session {
	c.mu.Lock()
	c.session.EndedAt = time.Now()
	for _, hs := range c.hops {
		hs.fold(math.MaxInt)
	}
	c.mu.Unlock()

	s := c.Snapshot(0)
	// Analyze recomputes the per-hop stats from the samples it can see; where a
	// trimmed run measured more than that, the accumulated figures stand (see
	// Session.Recompute).
	s.Analyze()
	return s
}
