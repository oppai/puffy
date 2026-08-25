// puffy measures a network path over time and draws it: round-trip time and
// packet loss as time series in the terminal, per hop, plus a JSON record and a
// self-contained HTML report of the same data.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/oppai/puffy/internal/model"
	"github.com/oppai/puffy/internal/probe"
	"github.com/oppai/puffy/internal/report"
	"github.com/oppai/puffy/internal/tui"
)

const usage = `puffy - ping and traceroute as time-series graphs

usage:
  puffy ping   [flags] <host>     round-trip time and loss over time
  puffy trace  [flags] <host>     per-hop round-trip time and loss over time
  puffy render [flags] <file>     turn a saved session into an HTML report
  puffy version

examples:
  puffy ping 8.8.8.8
  puffy trace github.com --interval 500ms
  puffy trace 1.1.1.1 --count 300 --json run.json --html run.html
  puffy ping example.com --json - | jq '.hops[0].stats'
  puffy render run.json -o report.html

puffy uses unprivileged ICMP sockets, so it normally needs no root. Run
"puffy ping --help" for the full flag list.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "ping", "trace":
		err = runProbe(ctx, os.Args[1], os.Args[2:])
	case "render":
		err = runRender(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("puffy %s\n", probe.Version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "puffy: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "puffy: %v\n", err)
		os.Exit(1)
	}
}

// reorder moves operands after flags. The standard flag package stops parsing
// at the first non-flag argument, but `puffy ping 8.8.8.8 -c 5` is how people
// actually type it, so the target is lifted out and re-appended.
func reorder(fs *flag.FlagSet, args []string) []string {
	var flags, operands []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			operands = append(operands, args[i+1:]...)
			return append(flags, operands...)
		case len(a) > 1 && a[0] == '-':
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") {
				continue // value is already attached
			}
			// A non-boolean flag takes the next argument with it. An unknown
			// flag is left alone so Parse can report it properly.
			if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		default:
			operands = append(operands, a)
		}
	}
	return append(flags, operands...)
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// options are the flags shared by ping and trace.
type options struct {
	interval time.Duration
	timeout  time.Duration
	count    int
	size     int
	ttl      int
	firstTTL int
	maxTTL   int
	v4, v6   bool
	priv     bool
	noDNS    bool
	window   int
	jsonPath string
	htmlPath string
	theme    string
	noColor  bool
	color    bool
	plain    bool
}

func runProbe(ctx context.Context, mode string, args []string) error {
	fs := flag.NewFlagSet("puffy "+mode, flag.ExitOnError)
	var o options

	fs.DurationVar(&o.interval, "interval", time.Second, "time between rounds of probes")
	fs.DurationVar(&o.interval, "i", time.Second, "shorthand for -interval")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Second, "how long to wait for a reply before scoring it lost")
	fs.DurationVar(&o.timeout, "W", 2*time.Second, "shorthand for -timeout")
	fs.IntVar(&o.count, "count", 0, "stop after this many rounds (0 runs until interrupted)")
	fs.IntVar(&o.count, "c", 0, "shorthand for -count")
	fs.IntVar(&o.size, "size", 56, "icmp payload size in bytes")
	fs.IntVar(&o.size, "s", 56, "shorthand for -size")
	fs.BoolVar(&o.v4, "4", false, "resolve the target to IPv4")
	fs.BoolVar(&o.v6, "6", false, "resolve the target to IPv6")
	fs.BoolVar(&o.priv, "privileged", false, "use a raw socket (needs root or CAP_NET_RAW)")
	fs.BoolVar(&o.noDNS, "no-dns", false, "skip reverse DNS lookups for hop names")
	fs.IntVar(&o.window, "window", 0, "rounds of history to graph (0 fits the terminal)")
	fs.StringVar(&o.jsonPath, "json", "", `write the session as JSON to this path ("-" for stdout)`)
	fs.StringVar(&o.htmlPath, "html", "", `write an HTML report to this path ("-" for stdout)`)
	fs.StringVar(&o.theme, "theme", "auto", "colour theme for the graphs: auto, dark or light")
	fs.BoolVar(&o.noColor, "no-color", false, "disable colour")
	fs.BoolVar(&o.color, "color", false, "force colour even when output is not a terminal")
	fs.BoolVar(&o.plain, "plain", false, "print one line per probe instead of the live graph")

	if mode == "ping" {
		fs.IntVar(&o.ttl, "ttl", 64, "ttl for the outgoing probes")
	} else {
		fs.IntVar(&o.firstTTL, "first-ttl", 1, "first ttl to probe")
		fs.IntVar(&o.maxTTL, "max-ttl", 30, "highest ttl to probe before giving up on reaching the target")
		fs.IntVar(&o.maxTTL, "m", 30, "shorthand for -max-ttl")
	}

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: puffy %s [flags] <host>\n\nflags:\n", mode)
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected exactly one target host")
	}
	if o.v4 && o.v6 {
		return errors.New("-4 and -6 are mutually exclusive")
	}
	target := fs.Arg(0)

	// Machine output owns stdout when it is aimed there, so the graphs and the
	// summary move to stderr rather than corrupting a piped document.
	humanOut := os.Stdout
	if o.jsonPath == "-" || o.htmlPath == "-" {
		humanOut = os.Stderr
	}

	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ip, err := probe.Resolve(resolveCtx, target, o.v4, o.v6)
	cancel()
	if err != nil {
		return err
	}

	cfg := probe.DefaultConfig()
	cfg.Mode = mode
	cfg.Target = target
	cfg.IP = ip
	cfg.Interval = o.interval
	cfg.Timeout = o.timeout
	cfg.Count = o.count
	cfg.PayloadSize = o.size
	cfg.Privileged = o.priv
	if mode == "ping" {
		cfg.TTL = o.ttl
	} else {
		cfg.FirstTTL = o.firstTTL
		cfg.MaxTTL = o.maxTTL
	}
	if cfg.Timeout < cfg.Interval {
		// A timeout shorter than the interval scores slow-but-alive hops as
		// lost, which is the single most misleading thing this tool could do.
		fmt.Fprintf(os.Stderr, "puffy: note: timeout %s is shorter than the interval %s, so slow replies will count as loss\n",
			cfg.Timeout, cfg.Interval)
	}

	eng, err := probe.New(cfg)
	if err != nil {
		return err
	}
	defer eng.Close()

	collector := probe.NewCollector(cfg, !o.noDNS)
	if target != ip.String() {
		collector.SetTargetName(target)
	}

	screen := tui.NewScreen(humanOut, o.theme, o.color, o.noColor)
	live := screen.IsTTY() && !o.plain
	view := tui.View{Screen: screen, Window: o.window}

	runErr := make(chan error, 1)
	go func() { runErr <- eng.Run(ctx) }()

	if live {
		screen.EnterAlt()
	} else if !o.plain {
		fmt.Fprintf(humanOut, "puffy %s %s (%s), every %s\n", mode, target, ip, o.interval)
	}

	// Redraw faster than the probe interval so a reply appears when it lands
	// rather than at the next round.
	tick := o.interval / 2
	if tick > 250*time.Millisecond {
		tick = 250 * time.Millisecond
	}
	if tick < 40*time.Millisecond {
		tick = 40 * time.Millisecond
	}
	redraw := time.NewTicker(tick)
	defer redraw.Stop()

	results := eng.Results()
	for results != nil {
		select {
		case r, ok := <-results:
			if !ok {
				results = nil
				continue
			}
			collector.Add(r)
			if o.plain {
				fmt.Fprintln(humanOut, plainLine(mode, r))
			}
		case <-redraw.C:
			if live {
				screen.Frame(view.Live(collector.Snapshot()))
			}
		}
	}

	if live {
		screen.LeaveAlt()
	}
	if err := <-runErr; err != nil {
		return err
	}

	session := collector.Finish()
	if session.Rounds == 0 {
		return errors.New("no probes completed")
	}
	if !o.plain || live {
		screen.Print(view.Summary(session))
	}

	return writeOutputs(session, o, humanOut)
}

func writeOutputs(session *model.Session, o options, humanOut *os.File) error {
	if o.jsonPath != "" {
		if err := report.WriteJSON(session, o.jsonPath, o.jsonPath != "-"); err != nil {
			return err
		}
		if o.jsonPath != "-" {
			fmt.Fprintf(humanOut, "\nwrote %s\n", o.jsonPath)
		}
	}
	if o.htmlPath != "" {
		if err := report.WriteHTML(session, o.htmlPath, o.theme); err != nil {
			return err
		}
		if o.htmlPath != "-" {
			fmt.Fprintf(humanOut, "wrote %s  (open it in a browser)\n", o.htmlPath)
		}
	}
	return nil
}

// plainLine is the one-line-per-probe form, close enough to ping(8) and
// traceroute(8) output to be greppable.
func plainLine(mode string, r probe.Result) string {
	from := "*"
	if r.From.IsValid() {
		from = r.From.String()
	}
	if !r.Kind.Replied() {
		if mode == "ping" {
			return fmt.Sprintf("round %d: no reply (timeout)", r.Round)
		}
		return fmt.Sprintf("round %d ttl %d: *", r.Round, r.TTL)
	}
	note := ""
	if r.Kind == probe.KindUnreachable {
		note = " (unreachable)"
	}
	return fmt.Sprintf("round %d ttl %d: %s %.3f ms%s",
		r.Round, r.TTL, from, float64(r.RTT)/float64(time.Millisecond), note)
}

func runRender(args []string) error {
	fs := flag.NewFlagSet("puffy render", flag.ExitOnError)
	out := fs.String("o", "", `where to write the HTML ("-" for stdout, default alongside the input)`)
	fs.StringVar(out, "output", "", "shorthand for -o")
	theme := fs.String("theme", "auto", "initial theme for the report: auto, dark or light")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: puffy render [flags] <session.json>\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("expected exactly one session file")
	}
	in := fs.Arg(0)

	session, err := report.ReadJSON(in)
	if err != nil {
		return err
	}
	// A stored session may predate the analysis pass, or have been trimmed by
	// hand, so the insights are recomputed rather than trusted.
	session.Analyze()

	dst := *out
	if dst == "" {
		if in == "-" {
			dst = "-"
		} else {
			dst = strings.TrimSuffix(in, filepath.Ext(in)) + ".html"
		}
	}
	if err := report.WriteHTML(session, dst, *theme); err != nil {
		return err
	}
	if dst != "-" {
		fmt.Printf("wrote %s  (%d hops, %d rounds)\n", dst, len(session.Hops), session.Rounds)
	}
	return nil
}
