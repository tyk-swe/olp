//go:build bench

// Command mockupstream serves the benchmark's deterministic LLM upstream.
//
//	mockupstream -addr 127.0.0.1:0 -ttft-ms 50 -interval-ms 20 -output-tokens 64 \
//	    -model 'primary:status=503' -model 'secondary:ttft_ms=5'
//
// It prints one JSON line on standard output once it listens, so a script can
// read the port it was given:
//
//	{"listener":"mock","address":"127.0.0.1:41233"}
//
// See package mockupstream for the dialects, the controls and the endpoints.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/tyk-swe/olp/tests/bench/mockupstream"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "mockupstream:", err)
		os.Exit(1)
	}
}

// models collects the repeatable -model flag.
type models []string

func (m *models) String() string     { return strings.Join(*m, " ") }
func (m *models) Set(v string) error { *m = append(*m, v); return nil }

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("mockupstream", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:0", "address to listen on; port 0 picks a free port, which is printed")
	configPath := fs.String("config", "", "JSON file with a configuration: {\"default\": {...}, \"models\": {\"name\": {...}}}")
	var def mockupstream.Rule
	var ttft, interval, delay float64
	var tokens, status, failAfter, failFirst int
	var failMode string
	fs.Float64Var(&ttft, "ttft-ms", 0, "default time to first token, in milliseconds")
	fs.Float64Var(&interval, "interval-ms", 0, "default interval between token frames, in milliseconds")
	fs.IntVar(&tokens, "output-tokens", 16, "default completion length in tokens")
	fs.IntVar(&status, "status", 0, "default status; 400 to 599 fail every request")
	fs.Float64Var(&delay, "error-delay-ms", 0, "default delay before an error response, in milliseconds")
	fs.IntVar(&failAfter, "fail-after-tokens", 0, "default: fail streams after this many tokens (0 never)")
	fs.StringVar(&failMode, "fail-mode", "abort", "how a failing stream ends: abort or error")
	fs.IntVar(&failFirst, "fail-first-n", 0, "default: fail the first N requests of each model with 503")
	var perModel models
	fs.Var(&perModel, "model", "per-model rule `name:key=value,...` with the keys ttft_ms, interval_ms, output_tokens, status, error_delay_ms, fail_after_tokens, fail_mode, fail_first_n; repeatable")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	// Only the flags that were given override the file, so the file's default
	// survives a flag left alone.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "ttft-ms":
			def.TTFTMs = &ttft
		case "interval-ms":
			def.IntervalMs = &interval
		case "output-tokens":
			def.OutputTokens = &tokens
		case "status":
			def.Status = &status
		case "error-delay-ms":
			def.ErrorDelayMs = &delay
		case "fail-after-tokens":
			def.FailAfterTokens = &failAfter
		case "fail-mode":
			def.FailMode = &failMode
		case "fail-first-n":
			def.FailFirstN = &failFirst
		}
	})
	var spec mockupstream.Spec
	if *configPath != "" {
		data, err := os.ReadFile(*configPath)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&spec); err != nil {
			return fmt.Errorf("%s: %w", *configPath, err)
		}
	}
	spec.Default = def.Overlay(spec.Default)
	for _, arg := range perModel {
		name, rule, ok := strings.Cut(arg, ":")
		if !ok || name == "" {
			return fmt.Errorf("-model %q: want name:key=value,...", arg)
		}
		parsed, err := mockupstream.ParseRule(rule)
		if err != nil {
			return fmt.Errorf("-model %q: %w", arg, err)
		}
		if spec.Models == nil {
			spec.Models = map[string]mockupstream.Rule{}
		}
		spec.Models[name] = parsed.Overlay(spec.Models[name])
	}
	cfg, err := spec.Config(mockupstream.Config{Default: mockupstream.DefaultBehavior()})
	if err != nil {
		return err
	}
	server, err := mockupstream.New(cfg)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	line, _ := json.Marshal(map[string]string{"listener": "mock", "address": ln.Addr().String()})
	fmt.Fprintf(stdout, "%s\n", line)
	return mockupstream.Serve(ctx, ln, server)
}
