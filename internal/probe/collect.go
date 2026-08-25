package probe

import (
	"context"
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

// Collector folds the engine's out-of-order result stream into a Session. It is
// safe for the renderer to read a snapshot while results keep arriving.
type Collector struct {
	mu      sync.Mutex
	session model.Session
	hops    map[int]*model.Hop
	// finalTTL is the shortest TTL that reached the destination. Hops past it
	// are dropped from the view: they are TTLs we probed before we knew where
	// the path ended, not real hops.
	finalTTL int
	maxRound int

	resolve  bool
	names    map[netip.Addr]string
	resolved map[netip.Addr]bool
}

// NewCollector starts a session document for the given configuration.
func NewCollector(cfg Config, resolveNames bool) *Collector {
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
		hops:     make(map[int]*model.Hop),
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

	hop, ok := c.hops[r.TTL]
	if !ok {
		hop = &model.Hop{TTL: r.TTL}
		c.hops[r.TTL] = hop
	}

	sample := model.Sample{
		Round:    r.Round,
		OffsetMS: float64(r.Sent.Sub(c.session.StartedAt)) / float64(time.Millisecond),
	}
	// An unreachable reply proves the hop is on the path and tells us how long
	// the round trip took, so it is a measurement, not a loss. The code is kept
	// in the insight layer rather than the sample.
	if r.Kind.Replied() {
		ms := float64(r.RTT) / float64(time.Millisecond)
		sample.RTTMS = &ms
		if r.From.IsValid() {
			sample.Addr = r.From.String()
			c.noteAddr(hop, r.From)
		}
	}
	insertSample(hop, sample)

	if r.Kind.Reached() {
		hop.Final = true
		if c.finalTTL == 0 || r.TTL < c.finalTTL {
			c.finalTTL = r.TTL
		}
	}
	if r.Round > c.maxRound {
		c.maxRound = r.Round
	}

	needLookup := c.resolve && r.From.IsValid() && !c.resolved[r.From]
	if needLookup {
		c.resolved[r.From] = true
	}
	c.mu.Unlock()

	if needLookup {
		go c.lookup(r.From)
	}
}

// noteAddr records an address seen at this hop. Several addresses at one TTL
// mean the path is load balanced, which is worth surfacing rather than hiding
// behind whichever router answered last.
func (c *Collector) noteAddr(hop *model.Hop, addr netip.Addr) {
	s := addr.String()
	for _, existing := range hop.Addrs {
		if existing == s {
			return
		}
	}
	hop.Addrs = append(hop.Addrs, s)
}

// insertSample appends in round order. Results arrive out of order - a fast hop
// in a later round can beat a slow one - and jitter plus the trailing-window
// views assume the slice is ordered, so a short backward scan pays for itself.
func insertSample(hop *model.Hop, s model.Sample) {
	i := len(hop.Samples)
	for i > 0 && hop.Samples[i-1].Round > s.Round {
		i--
	}
	if i == len(hop.Samples) {
		hop.Samples = append(hop.Samples, s)
		return
	}
	hop.Samples = append(hop.Samples, model.Sample{})
	copy(hop.Samples[i+1:], hop.Samples[i:])
	hop.Samples[i] = s
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
// Hops past the destination are omitted, hop names are filled in from the
// reverse-DNS cache, and the derived statistics are recomputed.
func (c *Collector) Snapshot() *model.Session {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.session
	s.Rounds = c.maxRound + 1
	s.EndedAt = time.Now()
	if !c.session.EndedAt.IsZero() {
		s.EndedAt = c.session.EndedAt
	}

	ttls := make([]int, 0, len(c.hops))
	for ttl := range c.hops {
		if c.finalTTL > 0 && ttl > c.finalTTL {
			continue
		}
		ttls = append(ttls, ttl)
	}
	sort.Ints(ttls)

	s.Hops = make([]*model.Hop, 0, len(ttls))
	for _, ttl := range ttls {
		src := c.hops[ttl]
		h := &model.Hop{
			TTL:     src.TTL,
			Final:   src.Final,
			Addrs:   append([]string(nil), src.Addrs...),
			Samples: append([]model.Sample(nil), src.Samples...),
		}
		if len(h.Addrs) > 0 {
			if addr, err := netip.ParseAddr(h.Addrs[0]); err == nil {
				h.Name = c.names[addr]
			}
		}
		s.Hops = append(s.Hops, h)
	}
	s.Recompute()
	return &s
}

// Finish stamps the end time and runs the analysis pass.
func (c *Collector) Finish() *model.Session {
	c.mu.Lock()
	c.session.EndedAt = time.Now()
	c.mu.Unlock()
	s := c.Snapshot()
	s.Analyze()
	return s
}
