// Package coordination owns the concrete GLIDE client lifecycle and transport
// outcome classification. Features own keys, scripts, stream formats, and retries.
package coordination

import (
	"context"
	"crypto/x509"
	"errors"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	glide "github.com/valkey-io/valkey-glide/go/v2"
	"github.com/valkey-io/valkey-glide/go/v2/config"
)

func Configuration(rawURL, caFile string, timeout time.Duration) (*config.ClientConfiguration, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("OLP_VALKEY_URL must be a redis:// or rediss:// URL without query or fragment")
	}
	port := 6379
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("invalid OLP_VALKEY_URL port")
		}
	}
	db := 0
	if u.Path != "" && u.Path != "/" {
		db, err = strconv.Atoi(strings.TrimPrefix(u.Path, "/"))
		if err != nil || db < 0 {
			return nil, errors.New("invalid OLP_VALKEY_URL database")
		}
	}
	advanced := config.NewAdvancedClientConfiguration().WithConnectionTimeout(timeout)
	if caFile != "" {
		if u.Scheme != "rediss" {
			return nil, errors.New("OLP_VALKEY_TLS_CA_FILE requires rediss://")
		}
		data, err := os.ReadFile(caFile)
		if err != nil || !x509.NewCertPool().AppendCertsFromPEM(data) {
			return nil, errors.New("OLP_VALKEY_TLS_CA_FILE must contain PEM trust roots")
		}
		advanced.WithTlsConfiguration(config.NewTlsConfiguration().WithRootCertificates(data))
	}
	c := config.NewClientConfiguration().
		WithAddress(&config.NodeAddress{Host: u.Hostname(), Port: port}).
		WithUseTLS(u.Scheme == "rediss").WithDatabaseId(db).
		WithRequestTimeout(timeout).WithAdvancedConfiguration(advanced).
		WithClientName("olp-go").WithLazyConnect(true)
	if u.User != nil {
		password, _ := u.User.Password()
		username := u.User.Username()
		if username == "" {
			username = "default"
		}
		c.WithCredentials(config.NewServerCredentials(username, password))
	}
	return c, nil
}

type Client struct {
	raw    *glide.Client
	mu     sync.Mutex
	closed bool
	calls  sync.WaitGroup
}

func Open(ctx context.Context, c *config.ClientConfiguration) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := glide.NewClient(c)
	if err != nil {
		return nil, errors.New("cannot create GLIDE client")
	}
	client := &Client{raw: raw}
	if err := client.Ping(ctx); err != nil {
		client.Close()
		return nil, errors.New("Valkey connection failed")
	}
	return client, nil
}

func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.raw.Close()
	c.calls.Wait()
}
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
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, &CommandError{Cause: glide.NewClosingError("client closed")}
	}
	c.calls.Add(1)
	c.mu.Unlock()
	// GLIDE 2.5.2 removes canceled calls from its pending registry, then starts
	// a goroutine waiting for their native response. Close cannot release those
	// removed calls. Keep native calls registered until completion/close and
	// enforce caller cancellation here, with one owned, buffered result channel.
	// The native request timeout still bounds commands; blocking consumers must
	// additionally use finite server-side BLOCK deadlines.
	type result struct {
		value any
		err   error
	}
	done := make(chan result, 1)
	args = slices.Clone(args)
	go func() {
		defer c.calls.Done()
		value, err := c.raw.CustomCommand(context.WithoutCancel(ctx), args)
		done <- result{value, err}
	}()
	var value any
	var err error
	select {
	case <-ctx.Done():
		return nil, &CommandError{Cause: ctx.Err(), Ambiguous: true}
	case response := <-done:
		value, err = response.value, response.err
	}
	if err == nil {
		return value, nil
	}
	var timeout *glide.TimeoutError
	var disconnected *glide.DisconnectError
	var connection *glide.ConnectionError
	var closing *glide.ClosingError
	ambiguous := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &timeout) || errors.As(err, &disconnected) || errors.As(err, &connection) || errors.As(err, &closing)
	return nil, &CommandError{Cause: err, Ambiguous: ambiguous}
}
