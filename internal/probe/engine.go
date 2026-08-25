package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// Probe identifiers live in the ICMP echo sequence number, never in the
// payload. Routers are only obliged to quote the offending IP header plus eight
// bytes - the bare ICMP header - inside a TimeExceeded, and real routers do
// exactly that, so any matching scheme that needs the payload back loses hops.
// The sequence number survives; the echo ID does not (Linux ping sockets
// rewrite it to the socket's port), which is why ID is only checked on raw
// sockets.

// magic marks puffy's payload so a packet capture is easy to read. Nothing
// depends on it arriving back.
var magic = []byte("puffy!")

type inflight struct {
	round int
	ttl   int
	sent  time.Time
	timer *time.Timer
}

// Engine drives the probing. Results are delivered on the channel returned by
// Results in completion order, which is not round order: a fast hop in round 5
// can land before a slow hop in round 4.
type Engine struct {
	cfg Config

	conn *icmp.PacketConn
	v4   *ipv4.PacketConn
	v6   *ipv6.PacketConn

	isV6    bool
	echoID  int
	matchID bool
	dst     net.Addr
	payload []byte

	// writeMu serialises TTL-set + write. The TTL is socket state, so a round's
	// probes must not interleave with each other.
	writeMu sync.Mutex
	curTTL  int

	mu      sync.Mutex
	seq     uint16
	pending map[uint16]*inflight
	// destTTL is the shortest TTL known to reach the destination. Once found,
	// the sweep stops there instead of probing to MaxTTL forever.
	destTTL int

	results chan Result
	closed  chan struct{}
	once    sync.Once
	// emitting counts results that have been claimed but not yet delivered.
	// A claim is registered while holding mu, and shutdown clears pending under
	// the same lock, so once shutdown has run no new claim can start and Wait
	// is guaranteed to return.
	emitting sync.WaitGroup
}

// New opens the ICMP socket. On macOS an unprivileged datagram socket both
// sends echo requests with a chosen TTL and receives TimeExceeded replies, so
// traceroute works without root. On Linux the same requires
// net.ipv4.ping_group_range to include the caller's group; otherwise pass
// Privileged (root or CAP_NET_RAW) and puffy falls back to a raw socket.
func New(cfg Config) (*Engine, error) {
	if cfg.Mode == "" {
		cfg.Mode = "ping"
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	e := &Engine{
		cfg:     cfg,
		isV6:    !cfg.IP.Is4(),
		echoID:  os.Getpid() & 0xffff,
		matchID: cfg.Privileged,
		pending: make(map[uint16]*inflight),
		results: make(chan Result, 256),
		closed:  make(chan struct{}),
		curTTL:  -1,
	}

	network, listen := e.networks()
	conn, err := icmp.ListenPacket(network, listen)
	if err != nil {
		return nil, e.openError(network, err)
	}
	e.conn = conn

	ip := net.IP(cfg.IP.AsSlice())
	if cfg.Privileged {
		e.dst = &net.IPAddr{IP: ip}
	} else {
		// Datagram ICMP sockets are addressed like UDP even though no UDP
		// header is involved; the port is ignored.
		e.dst = &net.UDPAddr{IP: ip}
	}

	if e.isV6 {
		e.v6 = conn.IPv6PacketConn()
		// Ask the kernel for the hop limit of inbound packets so we can tell a
		// reply apart from an unrelated one if we ever need to.
		_ = e.v6.SetControlMessage(ipv6.FlagHopLimit, true)
	} else {
		e.v4 = conn.IPv4PacketConn()
	}

	e.payload = make([]byte, cfg.PayloadSize)
	copy(e.payload, magic)

	go e.recvLoop()
	return e, nil
}

func (e *Engine) networks() (network, listen string) {
	switch {
	case e.isV6 && e.cfg.Privileged:
		return "ip6:ipv6-icmp", "::"
	case e.isV6:
		return "udp6", "::"
	case e.cfg.Privileged:
		return "ip4:icmp", "0.0.0.0"
	default:
		return "udp4", "0.0.0.0"
	}
}

// openError turns the kernel's refusal into advice, since "operation not
// permitted" on its own sends people to sudo when a sysctl is the real fix.
func (e *Engine) openError(network string, err error) error {
	if !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("open icmp socket (%s): %w", network, err)
	}
	if e.cfg.Privileged {
		return fmt.Errorf("open raw icmp socket: %w (raw sockets need root or CAP_NET_RAW: try sudo, or setcap cap_net_raw+ep on the puffy binary)", err)
	}
	return fmt.Errorf("open unprivileged icmp socket: %w\n"+
		"  on Linux, allow it for your group:  sudo sysctl -w net.ipv4.ping_group_range=\"0 2147483647\"\n"+
		"  or grant the capability once:       sudo setcap cap_net_raw+ep $(which puffy)\n"+
		"  or run with --privileged under sudo", err)
}

// Results is the stream of completed probes. It closes when Run returns and
// every outstanding probe has either answered or timed out.
func (e *Engine) Results() <-chan Result { return e.results }

// Close releases the socket. Safe to call more than once.
func (e *Engine) Close() error {
	var err error
	e.once.Do(func() {
		close(e.closed)
		err = e.conn.Close()
	})
	return err
}

// Run sends rounds of probes until the context is cancelled or Count rounds
// have completed. It blocks, and closes the Results channel before returning.
func (e *Engine) Run(ctx context.Context) error {
	// Ordering matters: Close unblocks the receive loop, then shutdown waits for
	// any result already claimed before closing the channel. Closing the channel
	// first would race a timer that is mid-emit and panic on send.
	defer e.shutdown()
	defer e.Close()

	ticker := time.NewTicker(e.cfg.Interval)
	defer ticker.Stop()

	for round := 0; ; round++ {
		if err := e.sendRound(ctx, round); err != nil {
			return err
		}
		if e.cfg.Count > 0 && round+1 >= e.cfg.Count {
			break
		}
		select {
		case <-ctx.Done():
			// Deliberately not an error: Ctrl-C is how an unbounded run ends,
			// and the caller still wants the session it has collected.
			e.drain()
			return nil
		case <-ticker.C:
		}
	}

	// Let the last round's probes finish rather than reporting them as lost.
	e.drain()
	return nil
}

// drain waits for outstanding probes to answer or time out, so the final round
// is not scored as loss just because the run ended.
func (e *Engine) drain() {
	deadline := time.NewTimer(e.cfg.Timeout + 250*time.Millisecond)
	defer deadline.Stop()
	for {
		e.mu.Lock()
		n := len(e.pending)
		e.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-deadline.C:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ttlsFor returns the TTLs to probe in this round.
func (e *Engine) ttlsFor() []int {
	if e.cfg.Mode == "ping" {
		return []int{e.cfg.TTL}
	}
	e.mu.Lock()
	last := e.cfg.MaxTTL
	if e.destTTL > 0 && e.destTTL < last {
		last = e.destTTL
	}
	e.mu.Unlock()

	ttls := make([]int, 0, last-e.cfg.FirstTTL+1)
	for ttl := e.cfg.FirstTTL; ttl <= last; ttl++ {
		ttls = append(ttls, ttl)
	}
	return ttls
}

// sendRound emits one probe per TTL, spread across the front of the interval.
// Bursting thirty packets at once is the fastest way to trip a router's ICMP
// rate limiter and measure loss that only puffy caused.
func (e *Engine) sendRound(ctx context.Context, round int) error {
	ttls := e.ttlsFor()
	stagger := time.Duration(0)
	if n := len(ttls); n > 1 {
		stagger = e.cfg.Interval / 2 / time.Duration(n)
		if stagger > 20*time.Millisecond {
			stagger = 20 * time.Millisecond
		}
	}

	for i, ttl := range ttls {
		if i > 0 && stagger > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(stagger):
			}
		}
		if err := e.send(round, ttl); err != nil {
			// A single failed write should not end the run: transient
			// ENOBUFS/EHOSTUNREACH is normal on a flapping link. Report it as a
			// lost probe and keep measuring.
			if isFatalWriteErr(err) {
				return fmt.Errorf("send ttl %d: %w", ttl, err)
			}
			// send() already un-registered the probe, so this result is not
			// claimed from pending; account for it directly.
			e.emitting.Add(1)
			e.emit(Result{Round: round, TTL: ttl, Sent: time.Now(), Kind: KindTimeout})
			e.emitting.Done()
		}
	}
	return nil
}

func (e *Engine) send(round, ttl int) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	if err := e.setTTL(ttl); err != nil {
		return err
	}

	e.mu.Lock()
	e.seq++
	seq := e.seq
	e.mu.Unlock()

	var msg icmp.Message
	if e.isV6 {
		msg.Type = ipv6.ICMPTypeEchoRequest
	} else {
		msg.Type = ipv4.ICMPTypeEcho
	}
	msg.Body = &icmp.Echo{ID: e.echoID, Seq: int(seq), Data: e.payload}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return err
	}

	fl := &inflight{round: round, ttl: ttl, sent: time.Now()}
	// Register before writing: a LAN reply can beat the bookkeeping otherwise.
	e.mu.Lock()
	e.pending[seq] = fl
	e.mu.Unlock()
	fl.timer = time.AfterFunc(e.cfg.Timeout, func() { e.expire(seq) })

	if _, err := e.conn.WriteTo(wire, e.dst); err != nil {
		e.mu.Lock()
		delete(e.pending, seq)
		e.mu.Unlock()
		fl.timer.Stop()
		return err
	}
	return nil
}

// setTTL sets the outbound TTL, skipping the syscall when it is already right -
// in ping mode that is every packet after the first.
func (e *Engine) setTTL(ttl int) error {
	if e.curTTL == ttl {
		return nil
	}
	var err error
	if e.isV6 {
		err = e.v6.SetHopLimit(ttl)
	} else {
		err = e.v4.SetTTL(ttl)
	}
	if err != nil {
		e.curTTL = -1
		return fmt.Errorf("set ttl %d: %w", ttl, err)
	}
	e.curTTL = ttl
	return nil
}

// claim removes the record for seq and reports whether the caller now owns
// delivering its result. Exactly one of the receive loop and the timeout timer
// can win, which is what stops a reply that arrives during its own timeout from
// being counted twice.
func (e *Engine) claim(seq uint16) (*inflight, bool) {
	e.mu.Lock()
	fl, ok := e.pending[seq]
	if ok {
		delete(e.pending, seq)
		e.emitting.Add(1)
	}
	e.mu.Unlock()
	return fl, ok
}

// shutdown stops the clock on every outstanding probe, waits for results
// already claimed to be delivered, and only then closes the channel.
func (e *Engine) shutdown() {
	e.mu.Lock()
	for seq, fl := range e.pending {
		fl.timer.Stop()
		delete(e.pending, seq)
	}
	e.mu.Unlock()
	e.emitting.Wait()
	close(e.results)
}

func (e *Engine) expire(seq uint16) {
	fl, ok := e.claim(seq)
	if !ok {
		return
	}
	defer e.emitting.Done()
	e.emit(Result{Round: fl.round, TTL: fl.ttl, Seq: seq, Sent: fl.sent, Kind: KindTimeout})
}

// emit delivers a result. The closed case is an escape hatch, not the normal
// path: if the consumer has stopped reading and the buffer is full, a blocked
// send would otherwise wedge shutdown's Wait forever.
func (e *Engine) emit(r Result) {
	select {
	case e.results <- r:
	case <-e.closed:
	}
}

func (e *Engine) recvLoop() {
	buf := make([]byte, 1500)
	for {
		n, peer, err := e.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-e.closed:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return // socket closed under us
		}
		now := time.Now()
		e.handle(buf[:n], peer, now)
	}
}

func (e *Engine) handle(b []byte, peer net.Addr, now time.Time) {
	proto := ipv4.ICMPTypeEchoReply.Protocol()
	if e.isV6 {
		proto = ipv6.ICMPTypeEchoReply.Protocol()
	}
	msg, err := icmp.ParseMessage(proto, b)
	if err != nil {
		return
	}

	var seq uint16
	var id int
	var kind Kind

	switch body := msg.Body.(type) {
	case *icmp.Echo:
		if !isEchoReply(msg.Type) {
			return // our own request looped back on a raw socket
		}
		seq, id, kind = uint16(body.Seq), body.ID, KindEcho
	case *icmp.TimeExceeded:
		seq, id, err = e.quoted(body.Data)
		kind = KindTimeExceeded
	case *icmp.DstUnreach:
		seq, id, err = e.quoted(body.Data)
		kind = KindUnreachable
	default:
		return
	}
	if err != nil {
		return
	}
	if e.matchID && id != e.echoID {
		return // raw socket: someone else's ICMP traffic
	}

	fl, ok := e.claim(seq)
	if !ok {
		return // duplicate, or a reply that arrived after its own timeout
	}
	defer e.emitting.Done()
	fl.timer.Stop()

	if kind == KindEcho {
		e.mu.Lock()
		if e.destTTL == 0 || fl.ttl < e.destTTL {
			e.destTTL = fl.ttl
		}
		e.mu.Unlock()
	}

	e.emit(Result{
		Round: fl.round,
		TTL:   fl.ttl,
		Seq:   seq,
		Sent:  fl.sent,
		RTT:   now.Sub(fl.sent),
		From:  addrOf(peer),
		Kind:  kind,
		Code:  msg.Code,
	})
}

// quoted pulls the echo ID and sequence out of the original datagram that a
// router echoed back inside an error message. Only the first eight bytes of the
// offending packet are guaranteed to be there, which is just enough.
func (e *Engine) quoted(data []byte) (seq uint16, id int, err error) {
	var off int
	if e.isV6 {
		// Fixed 40-byte IPv6 header; puffy's own probes never carry extension
		// headers, so the next header must be ICMPv6 for this to be ours.
		if len(data) < 48 {
			return 0, 0, errors.New("truncated icmpv6 quote")
		}
		if data[6] != byte(ipv6.ICMPTypeEchoRequest.Protocol()) {
			return 0, 0, errors.New("quoted packet is not icmpv6")
		}
		off = 40
		if data[off] != 128 { // ICMPv6 EchoRequest
			return 0, 0, errors.New("quoted packet is not an echo request")
		}
	} else {
		if len(data) < 20 {
			return 0, 0, errors.New("truncated ip quote")
		}
		ihl := int(data[0]&0x0f) * 4
		if ihl < 20 || len(data) < ihl+8 {
			return 0, 0, errors.New("truncated icmp quote")
		}
		if data[9] != byte(ipv4.ICMPTypeEcho.Protocol()) {
			return 0, 0, errors.New("quoted packet is not icmp")
		}
		off = ihl
		if data[off] != 8 { // ICMPv4 Echo
			return 0, 0, errors.New("quoted packet is not an echo request")
		}
	}
	id = int(binary.BigEndian.Uint16(data[off+4 : off+6]))
	seq = binary.BigEndian.Uint16(data[off+6 : off+8])
	return seq, id, nil
}

func isEchoReply(t icmp.Type) bool {
	return t == ipv4.ICMPTypeEchoReply || t == ipv6.ICMPTypeEchoReply
}

func addrOf(a net.Addr) netip.Addr {
	switch v := a.(type) {
	case *net.UDPAddr:
		if ip, ok := netip.AddrFromSlice(v.IP); ok {
			return ip.Unmap()
		}
	case *net.IPAddr:
		if ip, ok := netip.AddrFromSlice(v.IP); ok {
			return ip.Unmap()
		}
	}
	return netip.Addr{}
}

// isFatalWriteErr separates "this link is having a moment" from "this socket
// will never work". Only the latter should stop the run.
func isFatalWriteErr(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrPermission) ||
		errors.Is(err, os.ErrInvalid)
}
