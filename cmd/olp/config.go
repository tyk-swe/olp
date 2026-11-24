package main

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/staging"
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
	Mode                      Mode
	Hostname                  string
	DatabaseURL               string
	DatabaseReadURL           string
	Region                    string
	DatabaseMaxConnections    int
	ValkeyURL                 string
	ValkeyCAFile              string
	CacheValkeyURL            string
	CacheValkeyCAFile         string
	ListenAddr                string
	ObservabilityListenAddr   string
	PublicOrigin              string
	LocalLoginEnabled         bool
	GatewayCORSAllowedOrigins []string
	MaxInlineMediaItems       int
	MaxInlineMediaItemBytes   int64
	MaxInlineMediaTotalBytes  int64
	ConsoleDir                string
	MediaSpoolDir             string
	MediaSpoolCapacityBytes   int64
	AuthHMACKeyFile           string
	BootstrapTokenFile        string
	MasterKeyFile             string
	ConnectorConfigFile       string
	// UnconfinedPluginDir names the directory of the image that holds
	// unconfined plugin executables. Setting it enables the experimental
	// unconfined plugin tier, which nothing else can.
	UnconfinedPluginDir string
	RuntimeDatabaseRole string
	LogLevel            slog.Level
	RequestTimeout      time.Duration
	StartupTimeout      time.Duration
	ShutdownTimeout     time.Duration
	// Inference and provider egress bounds; names mirror the reference settings.
	TrustedProxyCIDRs             []netip.Prefix
	ManagementAllowedCIDRs        []netip.Prefix
	ProviderEgressAllowCIDRs      []netip.Prefix
	ProviderEgressAllowHTTPHosts  []string
	MaxConnections                int
	ConnectionMaxAgeSeconds       int
	ConnectionDrainTimeoutSeconds int
	MaxInFlightInference          int
	MaxInFlightManagement         int
	// A bounded admission queue holds inference work while the pool is full;
	// a depth of zero answers 503 at once.
	AdmissionQueueDepth      int
	AdmissionQueueTimeout    time.Duration
	MaxInFlightShadow        int
	MaxJSONBodyBytes         int64
	MaxMediaBodyBytes        int64
	ProviderMaxResponseBytes int64
	ProviderMaxEventBytes    int64
	// Optional OTLP/HTTP trace export. Credentials live only in the headers
	// file; ambient OTEL_* header variables are rejected at install time.
	OTLPTracesEndpoint     string
	OTLPHeadersFile        string
	TraceSampleRatio       float64
	TracePropagateUpstream bool
	TraceAcceptInbound     bool
	MetricsTenantLabels    []string
	MetricsSeriesCap       int
}

// Parse gives flags precedence over environment variables. File-backed URLs are
// mutually exclusive with inline URLs; their contents never appear in errors.
func Parse(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	return parseWithFlags(args, getenv, output, nil)
}

func parseWithFlags(args []string, getenv func(string) string, output io.Writer, extraFlags func(*flag.FlagSet)) (Config, error) {
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
	c.Hostname = getenv("HOSTNAME")
	f := flag.NewFlagSet("olp "+args[0], flag.ContinueOnError)
	f.SetOutput(output)
	v := registerConfigFlags(f, &c)
	var configurationFlags []*flag.Flag
	f.VisitAll(func(v *flag.Flag) { configurationFlags = append(configurationFlags, v) })
	if extraFlags != nil {
		extraFlags(f)
	}
	if err := f.Parse(args[1:]); err != nil {
		return c, err
	}
	if f.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	explicit := make(map[string]bool)
	f.Visit(func(v *flag.Flag) { explicit[v.Name] = true })
	var envErr error
	for _, v := range configurationFlags {
		if explicit[v.Name] {
			continue
		}
		name := "OLP_" + strings.ToUpper(strings.ReplaceAll(v.Name, "-", "_"))
		if value := getenv(name); value != "" {
			if err := f.Set(v.Name, value); err != nil {
				envErr = fmt.Errorf("invalid %s", name)
			}
		}
	}
	if envErr != nil {
		return c, envErr
	}
	return v.resolve(c, getenv)
}

func applyDatabasePassword(rawURL, path, endpoint string) (string, error) {
	databaseURL, err := url.Parse(rawURL)
	if err != nil || (databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql") || databaseURL.Hostname() == "" {
		return rawURL, nil
	}
	options := databaseURL.Query()
	if _, supplied := databaseURL.User.Password(); supplied || options.Has("password") || options.Has("passfile") {
		return rawURL, nil
	}
	config, err := pgconn.ParseConfig(rawURL)
	if err != nil {
		return "", errors.New("invalid OLP_DATABASE_URL")
	}
	if config.Password != "" {
		return rawURL, nil
	}
	if endpoint != "" {
		target, err := pgconn.ParseConfig(endpoint)
		if err != nil {
			return "", errors.New("invalid OLP_DATABASE_PASSWORD_FILE_URL")
		}
		if !matchesDatabaseEndpoint(config, target) {
			return rawURL, nil
		}
	}
	data, err := secrets.ReadRawFile(path)
	if err != nil {
		return "", fmt.Errorf("OLP_DATABASE_PASSWORD_FILE: %w", err)
	}
	defer clear(data)
	password := strings.TrimRight(string(data), "\n")
	if password == "" {
		return "", errors.New("OLP_DATABASE_PASSWORD_FILE must contain a password")
	}
	databaseURL.User = url.UserPassword(config.User, password)
	return databaseURL.String(), nil
}

func matchesDatabaseEndpoint(config, target *pgconn.Config) bool {
	if config.User != target.User || cmp.Or(config.Database, config.User) != cmp.Or(target.Database, target.User) ||
		config.Host != target.Host || config.Port != target.Port {
		return false
	}
	return !slices.ContainsFunc(config.Fallbacks, func(fallback *pgconn.FallbackConfig) bool {
		return fallback.Host != target.Host || fallback.Port != target.Port
	})
}

func parseCIDRs(name, raw string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for item := range strings.SplitSeq(raw, ",") {
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

func (c Config) spoolDirectory() string {
	return cmp.Or(c.MediaSpoolDir, filepath.Join(os.TempDir(), "olp-media-spool"))
}

func (c Config) Validate() error {
	if c.CacheValkeyCAFile != "" && c.CacheValkeyURL == "" {
		return errors.New("OLP_CACHE_VALKEY_TLS_CA_FILE requires OLP_CACHE_VALKEY_URL")
	}
	if c.CacheValkeyURL != "" {
		cacheURL, err := url.Parse(c.CacheValkeyURL)
		if err != nil || cacheURL.Hostname() == "" || (cacheURL.Scheme != "redis" && cacheURL.Scheme != "rediss") || cacheURL.RawQuery != "" || cacheURL.Fragment != "" {
			return errors.New("invalid OLP_CACHE_VALKEY_URL")
		}
		if c.CacheValkeyCAFile != "" && cacheURL.Scheme != "rediss" {
			return errors.New("cache TLS trust requires rediss")
		}
		mainURL, _ := url.Parse(c.ValkeyURL)
		port := func(u *url.URL) string {
			if u.Port() != "" {
				return u.Port()
			}
			return "6379"
		}
		if mainURL != nil && strings.EqualFold(cacheURL.Hostname(), mainURL.Hostname()) && port(cacheURL) == port(mainURL) {
			return errors.New("response cache requires a separate Valkey instance, not a logical database")
		}
	}

	if c.Region != "" {
		if len(c.Region) > 100 {
			return errors.New("OLP_REGION must be a valid region name")
		}
		for i, char := range c.Region {
			if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || i > 0 && (char == '.' || char == '_' || char == '-') {
				continue
			}
			return errors.New("OLP_REGION must be a valid region name")
		}
	}
	switch c.Mode {
	case All, Gateway, Control, Worker:
	default:
		return errors.New("invalid process mode")
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" {
		return errors.New("OLP_DATABASE_URL must be a PostgreSQL URL")
	}
	if c.DatabaseReadURL != "" {
		u, err := url.Parse(c.DatabaseReadURL)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" {
			return errors.New("OLP_DATABASE_READ_URL must be a PostgreSQL URL")
		}
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
	return c.validateServing()
}

func (c Config) validateServing() error {
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
	u, err := url.Parse(c.PublicOrigin)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("OLP_PUBLIC_ORIGIN must be an HTTP(S) origin")
	}
	if c.MaxConnections < 1 || c.MaxConnections > 100000 || c.ConnectionMaxAgeSeconds < 1 || c.ConnectionMaxAgeSeconds > 86400 || c.ConnectionDrainTimeoutSeconds < 1 || c.ConnectionDrainTimeoutSeconds > 600 {
		return errors.New("invalid HTTP connection capacity, maximum age, or drain timeout")
	}
	if c.MaxInFlightInference < 1 || c.MaxInFlightInference > 100000 {
		return errors.New("OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS must be between 1 and 100000")
	}
	if c.MaxInFlightManagement < 1 || c.MaxInFlightManagement > 100000 {
		return errors.New("OLP_HTTP_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS must be between 1 and 100000")
	}
	if c.AdmissionQueueDepth < 0 || c.AdmissionQueueDepth > 100000 {
		return errors.New("OLP_HTTP_ADMISSION_QUEUE_DEPTH must be between 0 and 100000")
	}
	if c.MaxInFlightShadow < 1 || c.MaxInFlightShadow > 100000 {
		return errors.New("OLP_HTTP_MAX_IN_FLIGHT_SHADOW_REQUESTS must be between 1 and 100000")
	}
	for _, origin := range c.GatewayCORSAllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || strings.Contains(origin, "*") {
			return errors.New("OLP_GATEWAY_CORS_ALLOWED_ORIGINS must contain exact HTTP(S) origins without paths or wildcards")
		}
	}
	if c.MaxInlineMediaItems < 1 || c.MaxInlineMediaItems > 64 || c.MaxInlineMediaItemBytes < 1024 || c.MaxInlineMediaItemBytes > c.MaxInlineMediaTotalBytes || c.MaxInlineMediaTotalBytes > 64<<20 {
		return errors.New("invalid inline media limits: require 1–64 items and 1 KiB <= item <= total <= 64 MiB")
	}
	if c.MaxJSONBodyBytes < 65536 || c.MaxJSONBodyBytes > 64<<20 {
		return errors.New("OLP_HTTP_MAX_JSON_BODY_BYTES must be between 64 KiB and 64 MiB")
	}
	if c.MediaSpoolCapacityBytes < staging.MinCapacityBytes {
		return errors.New("OLP_MEDIA_SPOOL_CAPACITY_BYTES must be at least 256 MiB")
	}
	if c.MaxMediaBodyBytes < 1<<20 || c.MaxMediaBodyBytes > bodylimit.Maximum {
		return errors.New("OLP_HTTP_MAX_MEDIA_BODY_BYTES must be between 1 MiB and 1 GiB")
	}
	if c.MaxMediaBodyBytes > c.MediaSpoolCapacityBytes/2 {
		return errors.New("OLP_HTTP_MAX_MEDIA_BODY_BYTES must stay within half of the media spool capacity")
	}
	if c.ProviderMaxResponseBytes < 1<<20 || c.ProviderMaxResponseBytes > 256<<20 {
		return errors.New("OLP_PROVIDER_MAX_RESPONSE_BYTES must be between 1 MiB and 256 MiB")
	}
	if c.ProviderMaxEventBytes < 65536 || c.ProviderMaxEventBytes > c.ProviderMaxResponseBytes {
		return errors.New("OLP_PROVIDER_MAX_EVENT_BYTES must be between 64 KiB and the response cap")
	}
	if c.TraceSampleRatio < 0 || c.TraceSampleRatio > 1 {
		return errors.New("OLP_TRACE_SAMPLE_RATIO must be between 0.0 and 1.0")
	}
	if c.MetricsSeriesCap < 64 || c.MetricsSeriesCap > 1000000 {
		return errors.New("OLP_METRICS_SERIES_CAP must be between 64 and 1000000")
	}
	if c.UnconfinedPluginDir != "" && !filepath.IsAbs(c.UnconfinedPluginDir) {
		return errors.New("OLP_UNCONFINED_PLUGIN_DIR must be an absolute path")
	}
	for _, setting := range []struct {
		name       string
		value, max time.Duration
	}{
		{"OLP_DEPENDENCY_REQUEST_TIMEOUT", c.RequestTimeout, time.Minute},
		{"OLP_STARTUP_TIMEOUT", c.StartupTimeout, time.Minute},
		{"OLP_SHUTDOWN_TIMEOUT", c.ShutdownTimeout, 10 * time.Minute},
		{"OLP_HTTP_ADMISSION_QUEUE_TIMEOUT", c.AdmissionQueueTimeout, time.Minute},
	} {
		if setting.value < time.Millisecond || setting.value > setting.max {
			return fmt.Errorf("%s must be between 1ms and %s", setting.name, setting.max)
		}
	}
	return nil
}
