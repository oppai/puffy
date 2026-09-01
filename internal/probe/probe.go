// Package probe is puffy's measurement engine. A ping is treated as a
// traceroute with a single fixed TTL, so both modes share one socket, one
// matching table and one timeout path.
package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Kind classifies what came back for a probe.
type Kind int

const (
	// KindTimeout means nothing came back before the deadline.
	KindTimeout Kind = iota
	// KindEcho means the destination itself answered: this TTL reaches it.
	KindEcho
	// KindTimeExceeded means an intermediate router answered.
	KindTimeExceeded
	// KindUnreachable means a router rejected the packet (admin prohibited,
	// host/net unreachable). The hop is on the path but forwarding failed.
	KindUnreachable
)

func (k Kind) String() string {
	switch k {
	case KindEcho:
		return "echo"
	case KindTimeExceeded:
		return "time-exceeded"
	case KindUnreachable:
		return "unreachable"
	default:
		return "timeout"
	}
}

// Reached reports whether this reply came from the destination.
func (k Kind) Reached() bool { return k == KindEcho }

// Replied reports whether any router answered at all.
func (k Kind) Replied() bool { return k != KindTimeout }

// Result is one completed probe.
type Result struct {
	Round int
	TTL   int
	Seq   uint16
	Sent  time.Time
	RTT   time.Duration
	From  netip.Addr
	Kind  Kind
	Code  int
}

// Config configures an Engine.
type Config struct {
	// Target is the name or literal the user asked for; IP is what it resolved to.
	Target string
	IP     netip.Addr

	Interval time.Duration
	Timeout  time.Duration

	// Mode is "ping" or "trace".
	Mode string

	// TTL is the fixed TTL used in ping mode.
	TTL int
	// FirstTTL and MaxTTL bound the sweep in trace mode.
	FirstTTL int
	MaxTTL   int

	// Count is the number of rounds to run; 0 runs until the context is done.
	Count int

	// PayloadSize is the ICMP payload length in bytes, matching ping's -s.
	PayloadSize int

	// History is how many trailing rounds of raw probe samples to keep. The
	// per-hop statistics are folded as probes arrive and stay exact for the whole
	// run whatever this is; what it bounds is the time series a long session can
	// draw and write out. 0 keeps everything, and lets an unattended run grow
	// until the machine complains.
	History int

	// Privileged opens a raw socket instead of the unprivileged datagram
	// socket. Raw sockets see every ICMP packet on the host, so replies must
	// then be filtered by echo ID.
	Privileged bool
}

// defaultHistory is the rolling raw history a run keeps when the caller does
// not choose: long enough that the graphs and the saved session cover hours of
// a real incident, short enough that an overnight trace stays in tens of
// megabytes instead of gigabytes.
const defaultHistory = 50000

// DefaultConfig returns the settings both subcommands start from.
func DefaultConfig() Config {
	return Config{
		Interval:    time.Second,
		Timeout:     2 * time.Second,
		TTL:         64,
		FirstTTL:    1,
		MaxTTL:      30,
		PayloadSize: 56,
		History:     defaultHistory,
	}
}

func (c *Config) validate() error {
	if !c.IP.IsValid() {
		return errors.New("no target address")
	}
	if c.Interval <= 0 {
		return errors.New("interval must be positive")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if c.PayloadSize < 0 || c.PayloadSize > 65000 {
		return fmt.Errorf("payload size %d out of range", c.PayloadSize)
	}
	if c.History < 0 {
		return fmt.Errorf("history %d rounds is negative", c.History)
	}
	switch c.Mode {
	case "ping":
		if c.TTL < 1 || c.TTL > 255 {
			return fmt.Errorf("ttl %d out of range", c.TTL)
		}
	case "trace":
		if c.FirstTTL < 1 || c.FirstTTL > 255 {
			return fmt.Errorf("first ttl %d out of range", c.FirstTTL)
		}
		if c.MaxTTL < c.FirstTTL || c.MaxTTL > 255 {
			return fmt.Errorf("max ttl %d out of range", c.MaxTTL)
		}
	default:
		return fmt.Errorf("unknown mode %q", c.Mode)
	}
	return nil
}

// Resolve turns a host or address literal into an address of the requested
// family. force4 and force6 correspond to the -4 and -6 flags.
func Resolve(ctx context.Context, host string, force4, force6 bool) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if force4 && !addr.Is4() {
			return netip.Addr{}, fmt.Errorf("%s is not an IPv4 address", host)
		}
		if force6 && !addr.Is6() {
			return netip.Addr{}, fmt.Errorf("%s is not an IPv6 address", host)
		}
		return addr.Unmap(), nil
	}
	network := "ip"
	switch {
	case force4:
		network = "ip4"
	case force6:
		network = "ip6"
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, network, host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return netip.Addr{}, fmt.Errorf("resolve %s: no addresses", host)
	}
	// Prefer IPv4 when the family is unconstrained: the unprivileged ICMP path
	// is the better tested one, and it matches what ping(8) does by default.
	if !force6 {
		for _, ip := range ips {
			if ip.Unmap().Is4() {
				return ip.Unmap(), nil
			}
		}
	}
	return ips[0].Unmap(), nil
}
