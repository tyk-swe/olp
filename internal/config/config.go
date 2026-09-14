// Package config parses process configuration without opening network connections.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/secrets"
)

type Mode string

const (
	All     Mode = "all"
	Gateway Mode = "gateway"
	Control Mode = "control"
	Worker  Mode = "worker"
)

func (m Mode) Public() bool     { return m != Worker }
func (m Mode) Management() bool { return m == All || m == Control }
func (m Mode) Inference() bool  { return m == All || m == Gateway }

type Config struct {
	Mode                    Mode
	DatabaseURL             string
	DatabaseMaxConnections  int
	ValkeyURL               string
	ValkeyCAFile            string
	ListenAddr              string
	ObservabilityListenAddr string
	PublicOrigin            string
	ConsoleDir              string
	AuthHMACKeyFile         string
	BootstrapTokenFile      string
	MasterKeyFile           string
	RuntimeDatabaseRole     string
	LogLevel                slog.Level
	RequestTimeout          time.Duration
	StartupTimeout          time.Duration
	ShutdownTimeout         time.Duration
	// Inference and provider egress bounds; names mirror the reference settings.
	TrustedProxyCIDRs            []netip.Prefix
	ProviderEgressAllowCIDRs     []netip.Prefix
	ProviderEgressAllowHTTPHosts []string
	MaxInFlightInference         int
	MaxJSONBodyBytes             int64
	ProviderMaxResponseBytes     int64
	ProviderMaxEventBytes        int64
}

// Parse gives flags precedence over environment variables. File-backed URLs are
// mutually exclusive with inline URLs; their contents never appear in errors.
func Parse(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	var c Config
	if len(args) == 0 {
		return c, errors.New("usage: olp <all|gateway|control|worker|health-probe> [flags]")
	}
	c.Mode = Mode(args[0])
	switch c.Mode {
	case All, Gateway, Control, Worker:
	default:
		return c, fmt.Errorf("unknown process mode %q", args[0])
	}
	f := flag.NewFlagSet("olp "+args[0], flag.ContinueOnError)
	f.SetOutput(output)
	var databaseFile, valkeyFile, level string
	f.StringVar(&c.DatabaseURL, "database-url", "", "PostgreSQL URL (OLP_DATABASE_URL)")
	f.StringVar(&databaseFile, "database-url-file", "", "file containing PostgreSQL URL")
	f.IntVar(&c.DatabaseMaxConnections, "database-max-connections", 20, "PostgreSQL pool capacity")
	f.StringVar(&c.ValkeyURL, "valkey-url", "", "redis:// or rediss:// URL; required for worker")
	f.StringVar(&valkeyFile, "valkey-url-file", "", "file containing Valkey URL")
	f.StringVar(&c.ValkeyCAFile, "valkey-tls-ca-file", "", "PEM trust roots for Valkey TLS")
	f.StringVar(&c.ListenAddr, "listen-addr", "127.0.0.1:8080", "public listener")
	f.StringVar(&c.ObservabilityListenAddr, "observability-listen-addr", "127.0.0.1:9090", "private health listener")
	f.StringVar(&c.PublicOrigin, "public-origin", "http://127.0.0.1:8080", "browser origin")
	f.StringVar(&c.ConsoleDir, "console-dir", "console/build", "static console directory")
	f.StringVar(&c.AuthHMACKeyFile, "auth-hmac-key-file", "", "mounted hex/base64 authentication key")
	f.StringVar(&c.BootstrapTokenFile, "bootstrap-token-file", "", "mounted first-owner bootstrap token")
	f.StringVar(&c.MasterKeyFile, "master-key-file", "", "mounted JSON master key ring")
	f.StringVar(&c.RuntimeDatabaseRole, "runtime-role", "", "existing runtime role granted access by migrate")
	f.StringVar(&level, "log-level", "info", "debug, info, warn, or error (OLP_LOG_LEVEL)")
	f.DurationVar(&c.RequestTimeout, "dependency-request-timeout", 2*time.Second, "dependency request deadline")
	f.DurationVar(&c.StartupTimeout, "startup-timeout", 10*time.Second, "startup deadline")
	f.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", 5*time.Second, "total shutdown deadline")
	var trustedProxies, egressCIDRs, egressHosts string
	f.StringVar(&trustedProxies, "trusted-proxy-cidrs", "", "comma-separated CIDRs allowed to supply X-Forwarded-For")
	f.StringVar(&egressCIDRs, "provider-egress-allow-cidrs", "", "comma-separated CIDRs exempt from the non-public provider egress denylist")
	f.StringVar(&egressHosts, "provider-egress-allow-http-hosts", "", "comma-separated hosts whose provider endpoints may use plain HTTP")
	f.IntVar(&c.MaxInFlightInference, "http-max-in-flight-inference-requests", 256, "inference work admission")
	f.Int64Var(&c.MaxJSONBodyBytes, "http-max-json-body-bytes", 2097152, "largest JSON request body, before and after gzip inflation")
	f.Int64Var(&c.ProviderMaxResponseBytes, "provider-max-response-bytes", 16777216, "largest buffered provider response body")
	f.Int64Var(&c.ProviderMaxEventBytes, "provider-max-event-bytes", 1048576, "largest single streamed provider event")
	if err := f.Parse(args[1:]); err != nil {
		return c, err
	}
	if f.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	explicit := make(map[string]bool)
	f.Visit(func(v *flag.Flag) { explicit[v.Name] = true })
	var envErr error
	f.VisitAll(func(v *flag.Flag) {
		if explicit[v.Name] {
			return
		}
		name := "OLP_" + strings.ToUpper(strings.ReplaceAll(v.Name, "-", "_"))
		if value := getenv(name); value != "" {
			if err := f.Set(v.Name, value); err != nil {
				envErr = fmt.Errorf("invalid %s", name)
			}
		}
	})
	if envErr != nil {
		return c, envErr
	}
	if getenv("OLP_OIDC_ALLOW_INSECURE_TEST_ISSUER") != "" || getenv("OLP_OIDC_ALLOW_PRIVATE_NETWORK") != "" {
		return c, errors.New("OIDC egress cannot be weakened by environment flags; use an explicit oidctest build for local issuer tests")
	}
	var err error
	if c.DatabaseURL, err = secretURL(c.DatabaseURL, databaseFile, "OLP_DATABASE_URL"); err != nil {
		return c, err
	}
	if c.ValkeyURL, err = secretURL(c.ValkeyURL, valkeyFile, "OLP_VALKEY_URL"); err != nil {
		return c, err
	}
	for name, path := range map[string]string{"OLP_AUTH_HMAC_KEY": c.AuthHMACKeyFile, "OLP_BOOTSTRAP_TOKEN": c.BootstrapTokenFile, "OLP_MASTER_KEY": c.MasterKeyFile} {
		if path != "" {
			if _, err := secretURL("", path, name); err != nil {
				return c, err
			}
		}
	}
	if c.TrustedProxyCIDRs, err = parseCIDRs("OLP_TRUSTED_PROXY_CIDRS", trustedProxies); err != nil {
		return c, err
	}
	if c.ProviderEgressAllowCIDRs, err = parseCIDRs("OLP_PROVIDER_EGRESS_ALLOW_CIDRS", egressCIDRs); err != nil {
		return c, err
	}
	for _, host := range strings.Split(egressHosts, ",") {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			c.ProviderEgressAllowHTTPHosts = append(c.ProviderEgressAllowHTTPHosts, host)
		}
	}
	if err = c.LogLevel.UnmarshalText([]byte(level)); err != nil {
		return c, errors.New("OLP_LOG_LEVEL must be debug, info, warn, or error")
	}
	if c.LogLevel != slog.LevelDebug && c.LogLevel != slog.LevelInfo && c.LogLevel != slog.LevelWarn && c.LogLevel != slog.LevelError {
		return c, errors.New("OLP_LOG_LEVEL must be debug, info, warn, or error")
	}
	return c, c.Validate()
}

func parseCIDRs(name, raw string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			prefix, err := netip.ParsePrefix(item)
			if err != nil {
				return nil, fmt.Errorf("%s must list CIDR prefixes", name)
			}
			prefixes = append(prefixes, prefix.Masked())
		}
	}
	return prefixes, nil
}

func secretURL(value, path, name string) (string, error) {
	if path == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("set only one of %s and %s_FILE", name, name)
	}
	data, err := secrets.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func (c Config) Validate() error {
	switch c.Mode {
	case All, Gateway, Control, Worker:
	default:
		return errors.New("invalid process mode")
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" {
		return errors.New("OLP_DATABASE_URL must be a PostgreSQL URL")
	}
	if c.DatabaseMaxConnections < 1 || c.DatabaseMaxConnections > 10000 {
		return errors.New("OLP_DATABASE_MAX_CONNECTIONS must be between 1 and 10000")
	}
	if c.Mode == Worker && c.ValkeyURL == "" {
		return errors.New("OLP_VALKEY_URL is required for worker")
	}
	if c.ValkeyCAFile != "" && c.ValkeyURL == "" {
		return errors.New("OLP_VALKEY_TLS_CA_FILE requires OLP_VALKEY_URL")
	}
	private, err := netip.ParseAddrPort(c.ObservabilityListenAddr)
	if err != nil {
		return errors.New("OLP_OBSERVABILITY_LISTEN_ADDR must be an IP:port")
	}
	if c.Mode.Public() {
		public, err := netip.ParseAddrPort(c.ListenAddr)
		if err != nil {
			return errors.New("OLP_LISTEN_ADDR must be an IP:port")
		}
		if public.Port() != 0 && public.Port() == private.Port() && (public.Addr() == private.Addr() || public.Addr().IsUnspecified() || private.Addr().IsUnspecified()) {
			return errors.New("public and private listeners must not overlap")
		}
	}
	u, err = url.Parse(c.PublicOrigin)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("OLP_PUBLIC_ORIGIN must be an HTTP(S) origin")
	}
	if c.MaxInFlightInference < 1 || c.MaxInFlightInference > 100000 {
		return errors.New("OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS must be between 1 and 100000")
	}
	if c.MaxJSONBodyBytes < 65536 || c.MaxJSONBodyBytes > 64<<20 {
		return errors.New("OLP_HTTP_MAX_JSON_BODY_BYTES must be between 64 KiB and 64 MiB")
	}
	if c.ProviderMaxResponseBytes < 1<<20 || c.ProviderMaxResponseBytes > 256<<20 {
		return errors.New("OLP_PROVIDER_MAX_RESPONSE_BYTES must be between 1 MiB and 256 MiB")
	}
	if c.ProviderMaxEventBytes < 65536 || c.ProviderMaxEventBytes > c.ProviderMaxResponseBytes {
		return errors.New("OLP_PROVIDER_MAX_EVENT_BYTES must be between 64 KiB and the response cap")
	}
	for _, setting := range []struct {
		name       string
		value, max time.Duration
	}{
		{"OLP_DEPENDENCY_REQUEST_TIMEOUT", c.RequestTimeout, time.Minute},
		{"OLP_STARTUP_TIMEOUT", c.StartupTimeout, time.Minute},
		{"OLP_SHUTDOWN_TIMEOUT", c.ShutdownTimeout, 10 * time.Minute},
	} {
		if setting.value < time.Millisecond || setting.value > setting.max {
			return fmt.Errorf("%s must be between 1ms and %s", setting.name, setting.max)
		}
	}
	return nil
}
