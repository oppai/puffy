package probe

import (
	"context"
	"encoding/binary"
	"math"
	"net/netip"
	"testing"
	"time"

	"github.com/oppai/puffy/internal/model"
)

// quotedV4 builds the payload a router puts inside an ICMPv4 error: an IP
// header of ihl*4 bytes followed by however much of the original datagram the
// router chose to include.
func quotedV4(ihl int, proto byte, id, seq uint16, extra int) []byte {
	b := make([]byte, ihl*4+8+extra)
	b[0] = 0x40 | byte(ihl) // version 4, header length in words
	b[9] = proto
	off := ihl * 4
	b[off] = 8 // ICMP Echo
	binary.BigEndian.PutUint16(b[off+4:], id)
	binary.BigEndian.PutUint16(b[off+6:], seq)
	return b
}

// Routers are only obliged to quote the IP header plus eight bytes, and real
// ones do exactly that, so matching has to work from that alone.
func TestQuotedV4MinimumQuote(t *testing.T) {
	e := &Engine{}
	seq, id, err := e.quoted(quotedV4(5, 1, 0xbeef, 0x1234, 0))
	if err != nil {
		t.Fatalf("28-byte quote rejected: %v", err)
	}
	if seq != 0x1234 || id != 0xbeef {
		t.Errorf("got seq=%#x id=%#x, want seq=0x1234 id=0xbeef", seq, id)
	}
}

// A header carrying IP options is longer than 20 bytes; the inner ICMP header
// must be found via IHL, not at a fixed offset.
func TestQuotedV4HonoursHeaderLength(t *testing.T) {
	e := &Engine{}
	seq, id, err := e.quoted(quotedV4(8, 1, 0x0a0b, 0x0c0d, 40))
	if err != nil {
		t.Fatalf("quote with IP options rejected: %v", err)
	}
	if seq != 0x0c0d || id != 0x0a0b {
		t.Errorf("got seq=%#x id=%#x, want seq=0x0c0d id=0x0a0b", seq, id)
	}
}

func TestQuotedV4Rejects(t *testing.T) {
	e := &Engine{}
	cases := map[string][]byte{
		"empty":            {},
		"truncated header": make([]byte, 12),
		"truncated icmp":   quotedV4(5, 1, 1, 1, 0)[:24],
		"not icmp":         quotedV4(5, 17, 1, 1, 0), // UDP
		"ihl below 5":      append([]byte{0x43}, make([]byte, 30)...),
	}
	for name, data := range cases {
		if _, _, err := e.quoted(data); err == nil {
			t.Errorf("%s: accepted a quote it should have rejected", name)
		}
	}
}

// The inner packet must be an echo *request* - our own probe coming back. A
// quoted reply, or some other host's traffic, is not ours to match.
func TestQuotedV4RejectsNonEchoRequest(t *testing.T) {
	e := &Engine{}
	b := quotedV4(5, 1, 1, 1, 0)
	b[20] = 0 // ICMP Echo Reply
	if _, _, err := e.quoted(b); err == nil {
		t.Error("accepted a quoted echo reply as our own probe")
	}
}

func TestQuotedV6(t *testing.T) {
	e := &Engine{isV6: true}
	b := make([]byte, 48)
	b[6] = 58 // next header: ICMPv6
	b[40] = 128
	binary.BigEndian.PutUint16(b[44:], 0xaaaa)
	binary.BigEndian.PutUint16(b[46:], 0x5555)
	seq, id, err := e.quoted(b)
	if err != nil {
		t.Fatalf("ipv6 quote rejected: %v", err)
	}
	if seq != 0x5555 || id != 0xaaaa {
		t.Errorf("got seq=%#x id=%#x, want seq=0x5555 id=0xaaaa", seq, id)
	}

	b[6] = 17 // UDP
	if _, _, err := e.quoted(b); err == nil {
		t.Error("accepted a non-ICMPv6 ipv6 quote")
	}
	if _, _, err := e.quoted(b[:40]); err == nil {
		t.Error("accepted a truncated ipv6 quote")
	}
}

func TestResolveLiterals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		host    string
		v4, v6  bool
		want    string
		wantErr bool
	}{
		{host: "8.8.8.8", want: "8.8.8.8"},
		{host: "8.8.8.8", v4: true, want: "8.8.8.8"},
		{host: "2001:4860:4860::8888", want: "2001:4860:4860::8888"},
		{host: "2001:4860:4860::8888", v6: true, want: "2001:4860:4860::8888"},
		{host: "8.8.8.8", v6: true, wantErr: true},
		{host: "2001:4860:4860::8888", v4: true, wantErr: true},
	}
	for _, tc := range cases {
		got, err := Resolve(ctx, tc.host, tc.v4, tc.v6)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Resolve(%q, v4=%v, v6=%v) = %v, want an error", tc.host, tc.v4, tc.v6, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Resolve(%q): %v", tc.host, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("Resolve(%q) = %s, want %s", tc.host, got, tc.want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	base := func() Config {
		c := DefaultConfig()
		c.Mode = "trace"
		c.IP = netip.MustParseAddr("192.0.2.1")
		return c
	}
	good := base()
	if err := good.validate(); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}

	bad := map[string]func(*Config){
		"no address":      func(c *Config) { c.IP = netip.Addr{} },
		"zero interval":   func(c *Config) { c.Interval = 0 },
		"zero timeout":    func(c *Config) { c.Timeout = 0 },
		"bad mode":        func(c *Config) { c.Mode = "wat" },
		"max below first": func(c *Config) { c.FirstTTL, c.MaxTTL = 20, 5 },
		"huge payload":    func(c *Config) { c.PayloadSize = 1 << 20 },
		"bad ping ttl":    func(c *Config) { c.Mode, c.TTL = "ping", 0 },
	}
	for name, mutate := range bad {
		c := base()
		mutate(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: config accepted", name)
		}
	}
}

// --- collector --------------------------------------------------------------

func testCollector(t *testing.T, mode string) *Collector {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Mode = mode
	cfg.Target = "192.0.2.1"
	cfg.IP = netip.MustParseAddr("192.0.2.1")
	return NewCollector(cfg, false)
}

func result(round, ttl int, from string, rtt time.Duration, kind Kind) Result {
	r := Result{Round: round, TTL: ttl, Sent: time.Now(), RTT: rtt, Kind: kind}
	if from != "" {
		r.From = netip.MustParseAddr(from)
	}
	return r
}

// Replies arrive in completion order, not round order: a fast hop in a later
// round beats a slow one. The per-hop series has to end up sorted anyway,
// because jitter and the trailing window both assume it.
func TestCollectorOrdersOutOfOrderResults(t *testing.T) {
	c := testCollector(t, "trace")
	c.Add(result(2, 1, "10.0.0.1", 3*time.Millisecond, KindTimeExceeded))
	c.Add(result(0, 1, "10.0.0.1", 1*time.Millisecond, KindTimeExceeded))
	c.Add(result(1, 1, "10.0.0.1", 2*time.Millisecond, KindTimeExceeded))

	hop := c.Snapshot(0).Hops[0]
	for i, s := range hop.Samples {
		if s.Round != i {
			t.Fatalf("samples out of order: %d at index %d", s.Round, i)
		}
	}
}

// Several addresses answering at one TTL means the path is load balanced. That
// is worth surfacing, not hiding behind whichever router answered last.
func TestCollectorRecordsEveryAddressAtAHop(t *testing.T) {
	c := testCollector(t, "trace")
	c.Add(result(0, 3, "10.0.0.1", time.Millisecond, KindTimeExceeded))
	c.Add(result(1, 3, "10.0.0.2", time.Millisecond, KindTimeExceeded))
	c.Add(result(2, 3, "10.0.0.1", time.Millisecond, KindTimeExceeded))

	hop := c.Snapshot(0).Hops[0]
	if len(hop.Addrs) != 2 {
		t.Errorf("addrs = %v, want the two distinct addresses", hop.Addrs)
	}
}

// TTLs past the destination were probed before we knew where the path ended.
// They are not hops, and must not appear as a tail of dead rows.
func TestCollectorDropsHopsPastDestination(t *testing.T) {
	c := testCollector(t, "trace")
	for ttl := 1; ttl <= 8; ttl++ {
		kind := KindTimeExceeded
		from := "10.0.0.1"
		if ttl >= 4 {
			kind, from = KindEcho, "192.0.2.1"
		}
		c.Add(result(0, ttl, from, time.Millisecond, kind))
	}
	s := c.Snapshot(0)
	if len(s.Hops) != 4 {
		t.Fatalf("kept %d hops, want 4 (the path ends at the first ttl that reached the target)", len(s.Hops))
	}
	if last := s.Hops[len(s.Hops)-1]; !last.Final {
		t.Error("the destination hop is not marked final")
	}
}

func TestCollectorScoresTimeoutAsLoss(t *testing.T) {
	c := testCollector(t, "ping")
	c.Add(result(0, 64, "192.0.2.1", 10*time.Millisecond, KindEcho))
	c.Add(result(1, 64, "", 0, KindTimeout))
	c.Add(result(2, 64, "192.0.2.1", 12*time.Millisecond, KindEcho))

	st := c.Snapshot(0).Hops[0].Stats
	if st.Sent != 3 || st.Recv != 2 || st.Lost != 1 {
		t.Errorf("sent/recv/lost = %d/%d/%d, want 3/2/1", st.Sent, st.Recv, st.Lost)
	}
}

// An unreachable reply proves the hop is on the path and gives a round-trip
// time, so it is a measurement rather than a loss.
func TestCollectorCountsUnreachableAsAReply(t *testing.T) {
	c := testCollector(t, "trace")
	c.Add(result(0, 5, "10.0.0.5", 7*time.Millisecond, KindUnreachable))
	st := c.Snapshot(0).Hops[0].Stats
	if st.Recv != 1 || st.Lost != 0 {
		t.Errorf("recv/lost = %d/%d, want 1/0", st.Recv, st.Lost)
	}
}

func TestCollectorFinishRunsAnalysis(t *testing.T) {
	c := testCollector(t, "trace")
	for r := range 12 {
		c.Add(result(r, 1, "10.0.0.1", 2*time.Millisecond, KindTimeExceeded))
		c.Add(result(r, 2, "192.0.2.1", 5*time.Millisecond, KindEcho))
	}
	s := c.Finish()
	if len(s.Insights) == 0 {
		t.Fatal("Finish produced no insights")
	}
	if s.Insights[0].Kind != model.InsightClean {
		t.Errorf("a clean run reported %q: %+v", s.Insights[0].Kind, s.Insights)
	}
	if s.EndedAt.Before(s.StartedAt) {
		t.Error("EndedAt precedes StartedAt")
	}
}

// --- end to end -------------------------------------------------------------

// The engine must reach the loopback and stop cleanly. This is the only test
// that touches a real socket; it is skipped where ICMP is not permitted.
func TestEngineAgainstLoopback(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = "ping"
	cfg.Target = "127.0.0.1"
	cfg.IP = netip.MustParseAddr("127.0.0.1")
	cfg.Interval = 20 * time.Millisecond
	cfg.Timeout = 500 * time.Millisecond
	cfg.Count = 3

	eng, err := New(cfg)
	if err != nil {
		t.Skipf("cannot open an icmp socket here: %v", err)
	}
	defer eng.Close()

	collector := NewCollector(cfg, false)
	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background()) }()

	for r := range eng.Results() {
		collector.Add(r)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	s := collector.Finish()
	if s.Rounds != 3 {
		t.Errorf("rounds = %d, want 3", s.Rounds)
	}
	if len(s.Hops) != 1 {
		t.Fatalf("ping produced %d hops, want 1", len(s.Hops))
	}
	if st := s.Hops[0].Stats; st.Sent != 3 {
		t.Errorf("sent = %d, want 3", st.Sent)
	}
}

// Cancelling mid-run must return the samples collected so far rather than an
// error: Ctrl-C is the normal way an unbounded run ends.
func TestEngineCancelIsNotAnError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = "ping"
	cfg.IP = netip.MustParseAddr("127.0.0.1")
	cfg.Interval = 20 * time.Millisecond
	cfg.Timeout = 300 * time.Millisecond

	eng, err := New(cfg)
	if err != nil {
		t.Skipf("cannot open an icmp socket here: %v", err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()
	for range eng.Results() {
	}
	if err := <-done; err != nil {
		t.Errorf("cancelled run returned an error: %v", err)
	}
}

// A run that lasts long enough to trim its raw history must not lose track of
// what it measured. The samples are a rolling window; the statistics are not.
func TestCollectorStatsStayExactAfterHistoryIsTrimmed(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = "ping"
	cfg.Target = "192.0.2.1"
	cfg.IP = netip.MustParseAddr("192.0.2.1")
	cfg.Interval = time.Second
	cfg.Timeout = 2 * time.Second
	cfg.History = 100
	c := NewCollector(cfg, false)

	const rounds = 5000
	sent, recv, lost := 0, 0, 0
	var sum, worst float64
	for r := range rounds {
		sent++
		if r%50 == 0 {
			lost++
			c.Add(result(r, 64, "", 0, KindTimeout))
			continue
		}
		recv++
		rtt := time.Duration(10+r%40) * time.Millisecond
		ms := float64(rtt) / float64(time.Millisecond)
		sum += ms
		worst = max(worst, ms)
		c.Add(result(r, 64, "192.0.2.1", rtt, KindEcho))
	}

	s := c.Finish()
	hop := s.Hops[0]
	st := hop.Stats
	if st.Sent != sent || st.Recv != recv || st.Lost != lost {
		t.Errorf("sent/recv/lost = %d/%d/%d, want %d/%d/%d over the whole run",
			st.Sent, st.Recv, st.Lost, sent, recv, lost)
	}
	if want := sum / float64(recv); math.Abs(st.Avg-want) > 1e-9 {
		t.Errorf("avg = %v, want %v", st.Avg, want)
	}
	if st.Worst != worst {
		t.Errorf("worst = %v, want %v", st.Worst, worst)
	}
	// And the raw history really is bounded, or none of the above would matter.
	if len(hop.Samples) > cfg.History+cfg.History/4 {
		t.Errorf("kept %d samples for a %d-round history", len(hop.Samples), cfg.History)
	}
	if s.SamplesFrom == 0 {
		t.Error("the session does not record that its oldest samples were dropped")
	}
	if got := s.Rounds - s.SamplesFrom; got != len(hop.Samples) {
		t.Errorf("retained span is %d rounds but %d samples were written", got, len(hop.Samples))
	}
}

// A redraw asks for the window it is about to draw, and must get that and not
// the run: this is what stops the frame cost growing with the session.
func TestCollectorSnapshotMaterialisesOnlyTheWindow(t *testing.T) {
	c := testCollector(t, "trace")
	for r := range 4000 {
		for ttl := 1; ttl <= 3; ttl++ {
			c.Add(result(r, ttl, "10.0.0.1", time.Millisecond, KindTimeExceeded))
		}
	}
	s := c.Snapshot(120)
	for _, h := range s.Hops {
		if len(h.Samples) != 120 {
			t.Errorf("ttl %d materialised %d samples for a 120-round window", h.TTL, len(h.Samples))
		}
		if first := h.Samples[0].Round; first != 4000-120 {
			t.Errorf("ttl %d window starts at round %d, want %d", h.TTL, first, 4000-120)
		}
		// The statistics still describe the whole run, not the window.
		if h.Stats.Sent != 4000 {
			t.Errorf("ttl %d reports %d probes sent, want all 4000", h.TTL, h.Stats.Sent)
		}
	}
	if s.SamplesFrom != 4000-120 {
		t.Errorf("SamplesFrom = %d, want %d", s.SamplesFrom, 4000-120)
	}
}

// A timeout only lands a whole timeout after it was sent, so a lossy hop's
// results arrive out of round order. Each one must be counted once, in the right
// place, and must still break the jitter chain.
func TestCollectorFoldsLateTimeoutsInRoundOrder(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = "ping"
	cfg.Target = "192.0.2.1"
	cfg.IP = netip.MustParseAddr("192.0.2.1")
	cfg.Interval = 200 * time.Millisecond
	cfg.Timeout = 2 * time.Second
	c := NewCollector(cfg, false)

	// Round 5's probe is lost, so it is only reported when its timeout fires -
	// 2s later, which at 200ms a round is ten rounds after it was sent. Every
	// other round replies immediately.
	for r := range 16 {
		if r == 5 {
			continue
		}
		c.Add(result(r, 64, "192.0.2.1", time.Duration(10+r)*time.Millisecond, KindEcho))
	}
	c.Add(result(5, 64, "", 0, KindTimeout))
	for r := 16; r < 40; r++ {
		c.Add(result(r, 64, "192.0.2.1", time.Duration(10+r)*time.Millisecond, KindEcho))
	}

	s := c.Finish()
	hop := s.Hops[0]
	if st := hop.Stats; st.Sent != 40 || st.Recv != 39 || st.Lost != 1 {
		t.Errorf("sent/recv/lost = %d/%d/%d, want 40/39/1", st.Sent, st.Recv, st.Lost)
	}
	for i, sm := range hop.Samples {
		if sm.Round != i {
			t.Fatalf("samples out of order: round %d at index %d", sm.Round, i)
		}
	}
	if !hop.Samples[5].Lost() {
		t.Error("the late timeout was not recorded at its own round")
	}
	// Rounds step by 1ms, so every consecutive pair differs by 1 - except the
	// pair either side of the lost round, which must not be paired at all.
	if j := hop.Stats.Jitter; math.Abs(j-1) > 1e-9 {
		t.Errorf("jitter = %v, want 1: the pair across the lost round was counted", j)
	}
}

// A result that arrives even later than a timeout can explain - the engine
// bounds it, but nothing in the collector may assume it - is still counted
// exactly once and still lands at its own round in the time series.
func TestCollectorCountsResultsThatArriveBeyondTheTimeoutBound(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = "ping"
	cfg.Target = "192.0.2.1"
	cfg.IP = netip.MustParseAddr("192.0.2.1")
	cfg.Interval = 200 * time.Millisecond
	cfg.Timeout = 400 * time.Millisecond // a lag of four rounds
	c := NewCollector(cfg, false)

	for r := range 40 {
		if r == 2 {
			continue
		}
		c.Add(result(r, 64, "192.0.2.1", 10*time.Millisecond, KindEcho))
	}
	c.Add(result(2, 64, "", 0, KindTimeout)) // 38 rounds late

	s := c.Finish()
	hop := s.Hops[0]
	if st := hop.Stats; st.Sent != 40 || st.Recv != 39 || st.Lost != 1 {
		t.Errorf("sent/recv/lost = %d/%d/%d, want 40/39/1", st.Sent, st.Recv, st.Lost)
	}
	if len(hop.Samples) != 40 || !hop.Samples[2].Lost() {
		t.Errorf("the very late timeout is not at round 2: %d samples", len(hop.Samples))
	}
}
