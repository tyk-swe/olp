package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tyk-swe/olp/internal/config"
	"github.com/tyk-swe/olp/internal/process"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "olp:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version":
			fmt.Println("olp 3.0.0 Go")
			return nil
		case "help", "--help", "-h":
			fmt.Println("usage: olp <all|gateway|control|worker|migrate|doctor|health-probe> [flags]\n       olp master-key <status|reencrypt> [flags]")
			return nil
		case "migrate", "doctor", "master-key":
			command, options := args[0], args[1:]
			if command == "master-key" {
				if len(options) == 0 || (options[0] != "status" && options[0] != "reencrypt") {
					return errors.New("usage: olp master-key <status|reencrypt> [flags]")
				}
				command, options = options[0], options[1:]
			}
			c, err := config.Parse(append([]string{"all"}, options...), os.Getenv, os.Stderr)
			if err != nil {
				return err
			}
			return process.Maintenance(ctx, c, command, os.Stdout)
		case "health-probe":
			if len(args) != 1 {
				return errors.New("health-probe takes no arguments")
			}
			return healthProbe(ctx)
		case "internal-pre-stop":
			f := flag.NewFlagSet(args[0], flag.ContinueOnError)
			seconds := f.Uint("seconds", 10, "pre-stop delay")
			if err := f.Parse(args[1:]); err != nil {
				return err
			}
			if f.NArg() != 0 || *seconds > 3600 {
				return errors.New("invalid pre-stop delay")
			}
			timer := time.NewTimer(time.Duration(*seconds) * time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	c, err := config.Parse(args, os.Getenv, os.Stderr)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: c.LogLevel}))
	return process.Run(ctx, c, log)
}

func healthProbe(ctx context.Context) error {
	address := os.Getenv("OLP_OBSERVABILITY_LISTEN_ADDR")
	if address == "" {
		address = "127.0.0.1:9090"
	}
	parsed, err := netip.ParseAddrPort(address)
	if err != nil {
		return errors.New("invalid OLP_OBSERVABILITY_LISTEN_ADDR")
	}
	if parsed.Addr().IsUnspecified() {
		loopback := netip.MustParseAddr("127.0.0.1")
		if parsed.Addr().Is6() {
			loopback = netip.IPv6Loopback()
		}
		parsed = netip.AddrPortFrom(loopback, parsed.Port())
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+parsed.String()+"/health/ready", nil)
	if err != nil {
		return err
	}
	transport := &http.Transport{DialContext: (&net.Dialer{}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("readiness probe failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness probe returned %d", resp.StatusCode)
	}
	return nil
}
