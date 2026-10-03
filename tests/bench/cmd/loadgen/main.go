//go:build bench

// Command loadgen drives an open-loop, constant-rate load against any LLM
// endpoint and reports latency from each request's scheduled send time.
//
//	loadgen -url http://127.0.0.1:8080 -dialect openai -api-key olp_... -model team-chat \
//	    -rate 1000 -duration 60s -warmup 10s -json .local/bench/s1.json
//
// The same command measures OLP, LiteLLM and the mock upstream directly:
// point -url at each and compare the reports. It prints a summary to standard
// error and, with -json, the full report as JSON. The exit status is 0 for a
// completed run, 1 for a usage or setup error, and 2 for a run that could not
// deliver its load (it dropped or sent late) when -strict is set.
//
// See package loadgen for what the numbers mean.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tyk-swe/olp/tests/bench/loadgen"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	var invalid *invalidRun
	switch {
	case err == nil:
	case errors.As(err, &invalid):
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

// invalidRun is the error of a completed run that could not be trusted.
type invalidRun struct{ problems []string }

func (e *invalidRun) Error() string {
	return "the run is invalid: " + strings.Join(e.problems, "; ")
}

// headers collects the repeatable -header flag.
type headers map[string]string

func (h headers) String() string { return fmt.Sprint(map[string]string(h)) }
func (h headers) Set(v string) error {
	name, value, ok := strings.Cut(v, ":")
	if !ok || strings.TrimSpace(name) == "" {
		return fmt.Errorf("%q is not Name: value", v)
	}
	h[strings.TrimSpace(name)] = strings.TrimSpace(value)
	return nil
}

// parseSizes reads a comma-separated list of prompt sizes in tokens, where a
// k suffix means thousands: "0", "50000,75000,100000" or "50k,75k,100k".
func parseSizes(s string) ([]int, error) {
	var sizes []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		scale := 1
		if rest, ok := strings.CutSuffix(part, "k"); ok {
			part, scale = rest, 1000
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("prompt size %q is not a non-negative number of tokens", part)
		}
		sizes = append(sizes, n*scale)
	}
	return sizes, nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg loadgen.Config
	extra := headers{}
	fs.StringVar(&cfg.URL, "url", "http://127.0.0.1:8080", "base `URL` of the target; the dialect's path is appended")
	fs.StringVar(&cfg.Path, "path", "", "request path replacing the dialect's; {model} and {operation} are substituted")
	dialect := fs.String("dialect", "openai", "wire dialect: openai, anthropic or gemini")
	fs.StringVar(&cfg.APIKey, "api-key", "", "API key, sent in the dialect's own header (default $LOADGEN_API_KEY, so it stays out of the process list)")
	fs.StringVar(&cfg.Model, "model", "", "model or route slug to request (required)")
	fs.Float64Var(&cfg.Rate, "rate", 100, "target arrival rate, requests per second")
	fs.DurationVar(&cfg.Duration, "duration", 30*time.Second, "measured period")
	fs.DurationVar(&cfg.Warmup, "warmup", 5*time.Second, "warmup at the same rate, excluded from the statistics")
	stream := fs.Bool("stream", false, "stream every response (shorthand for -stream-share 1)")
	fs.Float64Var(&cfg.StreamShare, "stream-share", 0, "fraction of requests that stream, from 0 to 1, spread evenly")
	sizes := fs.String("prompt-tokens", "0", "prompt sizes in tokens, used in equal shares in rotation: 0 is a short prompt, 50k,75k,100k the high-throughput mix")
	fs.IntVar(&cfg.MaxTokens, "max-tokens", loadgen.DefaultMaxTokens, "completion limit each request asks for")
	fs.IntVar(&cfg.MaxInFlight, "max-in-flight", loadgen.DefaultMaxInFlight, "concurrent request cap")
	fs.IntVar(&cfg.MaxBacklog, "max-backlog", 0, "requests allowed to wait for a slot: 0 same as -max-in-flight, negative none (drop at once)")
	fs.DurationVar(&cfg.Timeout, "timeout", loadgen.DefaultTimeout, "per-request timeout from its send; streams lasting longer than this fail")
	fs.DurationVar(&cfg.Drain, "drain", 0, "how long to wait for requests still running when the schedule ends (default -timeout plus one second)")
	fs.DurationVar(&cfg.LateAfter, "late-after", loadgen.DefaultLateAfter, "a send later than this after its scheduled time counts as late")
	fs.IntVar(&cfg.SlowRead.BytesPerSecond, "slow-read-bps", 0, "read each stream at this many bytes per second (0 reads at full speed)")
	fs.IntVar(&cfg.SlowRead.RcvBuf, "slow-read-rcvbuf", 0, "receive buffer of slow-reader connections in bytes (default 4096)")
	fs.BoolVar(&cfg.H2C, "h2c", false, "speak HTTP/2 without TLS, with prior knowledge")
	fs.Var(extra, "header", "extra request header `\"Name: value\"`; repeatable, e.g. x-mock-ttft-ms: 50 for a direct run against the mock")
	jsonPath := fs.String("json", "", "write the full report as JSON to this `path` (- for standard output)")
	name := fs.String("name", "", "label stored in the report")
	quiet := fs.Bool("quiet", false, "do not print the summary")
	strict := fs.Bool("strict", false, "exit with status 2 when the run is invalid")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	cfg.Dialect = loadgen.Dialect(*dialect)
	cfg.Headers = extra
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv("LOADGEN_API_KEY")
	}
	if *stream {
		cfg.StreamShare = 1
	}
	var err error
	if cfg.PromptTokens, err = parseSizes(*sizes); err != nil {
		return err
	}

	report, err := loadgen.Run(ctx, cfg)
	if err != nil {
		return err
	}
	report.Name = *name
	if !*quiet {
		fmt.Fprint(stderr, report.Text())
	}
	switch *jsonPath {
	case "":
	case "-":
		data, err := report.JSON()
		if err != nil {
			return err
		}
		if _, err = stdout.Write(data); err != nil {
			return err
		}
	default:
		if err := report.WriteJSON(*jsonPath); err != nil {
			return err
		}
	}
	if *strict && !report.Valid {
		return &invalidRun{report.Problems}
	}
	return nil
}
