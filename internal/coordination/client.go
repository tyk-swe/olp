// Package coordination owns the concrete Valkey client lifecycle and transport
// outcome classification. Features own keys, scripts, stream formats, and retries.
package coordination

import (
	"context"
	"crypto/x509"
	"errors"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"
)

type Config struct {
	option  valkey.ClientOption
	timeout time.Duration
}

func Configuration(rawURL, caFile string, timeout time.Duration) (Config, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, errors.New("OLP_VALKEY_URL must be a redis:// or rediss:// URL without query or fragment")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return Config{}, errors.New("invalid OLP_VALKEY_URL port")
		}
	}
	option, err := valkey.ParseURL(rawURL)
	if err != nil || option.SelectDB < 0 {
		return Config{}, errors.New("invalid OLP_VALKEY_URL database")
	}
	option.ClientName = "olp"
	option.ForceSingleClient = true
	option.DisableCache = true
	option.Dialer.Timeout = timeout
	option.ConnWriteTimeout = timeout
	if caFile == "" {
		return Config{option: option, timeout: timeout}, nil
	}
	if u.Scheme != "rediss" {
		return Config{}, errors.New("OLP_VALKEY_TLS_CA_FILE requires rediss://")
	}
	data, err := os.ReadFile(caFile)
	roots := x509.NewCertPool()
	if err != nil || !roots.AppendCertsFromPEM(data) {
		return Config{}, errors.New("OLP_VALKEY_TLS_CA_FILE must contain PEM trust roots")
	}
	option.TLSConfig.RootCAs = roots
	return Config{option: option, timeout: timeout}, nil
}

type Client struct {
	raw     valkey.Client
	timeout time.Duration
}

func Open(ctx context.Context, c Config) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type opened struct {
		raw valkey.Client
		err error
	}
	done := make(chan opened, 1)
	go func() {
		raw, err := valkey.NewClient(c.option)
		done <- opened{raw, err}
	}()
	select {
	case <-ctx.Done():
		go func() {
			if late := <-done; late.err == nil {
				late.raw.Close()
			}
		}()
		return nil, ctx.Err()
	case result := <-done:
		if result.err != nil {
			return nil, errors.New("Valkey connection failed")
		}
		return &Client{raw: result.raw, timeout: c.timeout}, nil
	}
}

func (c *Client) Close()                         { c.raw.Close() }
func (c *Client) Ping(ctx context.Context) error { _, err := c.Do(ctx, "PING"); return err }

// CommandError deliberately omits command arguments and server error text from
// logs. Unwrap retains the cause for callers. Ambiguous means the command may
// have executed: callers must reconcile with their idempotency protocol.
type CommandError struct {
	Cause     error
	Ambiguous bool
}

func (e *CommandError) Error() string {
	if e.Ambiguous {
		return "Valkey command failed; execution outcome is unknown"
	}
	return "Valkey command rejected"
}
func (e *CommandError) Unwrap() error { return e.Cause }

func (c *Client) Do(ctx context.Context, args ...string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, &CommandError{Cause: err}
	}
	command, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return reply(c.raw.Do(command, c.raw.B().Arbitrary(args...).Build()))
}

// DoBlocking runs a command that waits server side, such as XREADGROUP BLOCK,
// on a dedicated connection so it cannot stall the shared pipelined one. The
// caller's context bounds it in place of the request timeout.
func (c *Client) DoBlocking(ctx context.Context, args ...string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, &CommandError{Cause: err}
	}
	return reply(c.raw.Do(ctx, c.raw.B().Arbitrary(args...).Blocking()))
}

// Pipeline sends commands in one round trip, without atomicity, and returns
// their replies in order. A command the server refuses is reported in its
// place as an error value, so one bad reply leaves the others readable.
func (c *Client) Pipeline(ctx context.Context, commands ...[]string) ([]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, &CommandError{Cause: err}
	}
	built := make(valkey.Commands, len(commands))
	for index, command := range commands {
		built[index] = c.raw.B().Arbitrary(command...).Build()
	}
	command, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	results := c.raw.DoMulti(command, built...)
	values := make([]any, len(results))
	for index, result := range results {
		value, err := result.ToAny()
		refusal, refused := valkey.IsValkeyErr(err)
		switch {
		case err == nil || valkey.IsValkeyNil(err):
			values[index] = value
		case refused:
			values[index] = refusal
		default:
			return nil, &CommandError{Cause: err, Ambiguous: true}
		}
	}
	return values, nil
}

func reply(result valkey.ValkeyResult) (any, error) {
	value, err := result.ToAny()
	if err == nil || valkey.IsValkeyNil(err) {
		return value, nil
	}
	_, refused := valkey.IsValkeyErr(err)
	return nil, &CommandError{Cause: err, Ambiguous: !refused}
}
