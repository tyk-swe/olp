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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/telemetry"
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
	DatabaseURL               string
	DatabaseReadURL           string
	Region                    string
	DatabaseMaxConnections    int
	ValkeyURL                 string
	ValkeyCAFile              string
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
	var databaseFile, databaseReadFile, valkeyFile, level string
	f.StringVar(&c.DatabaseURL, "database-url", "", "PostgreSQL URL (OLP_DATABASE_URL)")
	f.StringVar(&databaseFile, "database-url-file", "", "file containing PostgreSQL URL")
	f.StringVar(&c.DatabaseReadURL, "database-read-url", "", "optional PostgreSQL replica for runtime release and authority reads")
	f.StringVar(&c.Region, "region", "", "deployment region for regional limits and local provider preference")
	f.StringVar(&databaseReadFile, "database-read-url-file", "", "file containing optional PostgreSQL read-replica URL")
	f.IntVar(&c.DatabaseMaxConnections, "database-max-connections", 20, "PostgreSQL pool capacity")
	f.StringVar(&c.ValkeyURL, "valkey-url", "", "redis:// or rediss:// URL; required for worker")
	f.StringVar(&valkeyFile, "valkey-url-file", "", "file containing Valkey URL")
	f.StringVar(&c.ValkeyCAFile, "valkey-tls-ca-file", "", "PEM trust roots for Valkey TLS")
	f.StringVar(&c.ListenAddr, "listen-addr", "127.0.0.1:8080", "public listener")
	f.StringVar(&c.ObservabilityListenAddr, "observability-listen-addr", "127.0.0.1:9090", "private health listener")
	f.StringVar(&c.PublicOrigin, "public-origin", "http://127.0.0.1:8080", "browser origin")
	f.StringVar(&c.ConsoleDir, "console-dir", "console/build", "static console directory")
	f.StringVar(&c.MediaSpoolDir, "media-spool-dir", "", "directory for bounded media staging; defaults to the system temp directory")
	f.Int64Var(&c.MediaSpoolCapacityBytes, "media-spool-capacity-bytes", 1073741824, "media spool capacity in bytes")
	f.StringVar(&c.AuthHMACKeyFile, "auth-hmac-key-file", "", "mounted hex/base64 authentication key")
	f.StringVar(&c.BootstrapTokenFile, "bootstrap-token-file", "", "mounted first-owner bootstrap token")
	f.StringVar(&c.ConnectorConfigFile, "connector-config-file", "", "mounted provider transport configuration")
	f.StringVar(&c.UnconfinedPluginDir, "unconfined-plugin-dir", "", "absolute directory of unconfined plugin executables; setting it enables the experimental unconfined plugin tier")
	f.StringVar(&c.MasterKeyFile, "master-key-file", "", "mounted JSON master key ring")
	f.StringVar(&c.RuntimeDatabaseRole, "runtime-role", "", "existing runtime role granted access by migrate")
	f.StringVar(&level, "log-level", "info", "debug, info, warn, or error (OLP_LOG_LEVEL)")
	f.DurationVar(&c.RequestTimeout, "dependency-request-timeout", 2*time.Second, "dependency request deadline")
	f.DurationVar(&c.StartupTimeout, "startup-timeout", 10*time.Second, "startup deadline")
	f.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", 30*time.Second, "total shutdown deadline")
	var trustedProxies, managementCIDRs, egressCIDRs, egressHosts, corsOrigins string
	f.BoolVar(&c.LocalLoginEnabled, "local-login-enabled", true, "allow password sign-in")
	f.StringVar(&corsOrigins, "gateway-cors-allowed-origins", "", "comma-separated origins allowed to call the inference API")
	f.IntVar(&c.MaxInlineMediaItems, "http-max-inline-media-items", 4, "maximum inline media items")
	f.Int64Var(&c.MaxInlineMediaItemBytes, "http-max-inline-media-item-bytes", 1048576, "maximum decoded bytes per inline media item")
	f.Int64Var(&c.MaxInlineMediaTotalBytes, "http-max-inline-media-total-bytes", 2097152, "maximum decoded inline media bytes per request")
	f.StringVar(&trustedProxies, "trusted-proxy-cidrs", "", "comma-separated CIDRs allowed to supply X-Forwarded-For")
	f.StringVar(&managementCIDRs, "management-allowed-cidrs", "", "comma-separated client CIDRs allowed to reach management and console; empty allows all")
	f.StringVar(&egressCIDRs, "provider-egress-allow-cidrs", "", "comma-separated CIDRs exempt from the non-public provider egress denylist")
	f.StringVar(&egressHosts, "provider-egress-allow-http-hosts", "", "comma-separated hosts whose provider endpoints may use plain HTTP")
	f.IntVar(&c.MaxConnections, "http-max-connections", 1024, "maximum accepted connections per listener")
	f.IntVar(&c.ConnectionMaxAgeSeconds, "http-connection-max-age-seconds", 300, "connection lifetime before graceful drain")
	f.IntVar(&c.ConnectionDrainTimeoutSeconds, "http-connection-drain-timeout-seconds", 30, "maximum age drain time")
	f.IntVar(&c.MaxInFlightInference, "http-max-in-flight-inference-requests", 256, "inference work admission")
	f.IntVar(&c.MaxInFlightManagement, "http-max-in-flight-management-requests", 32, "management and console request admission")
	f.IntVar(&c.AdmissionQueueDepth, "http-admission-queue-depth", 0, "inference requests that may wait for admission by priority")
	f.DurationVar(&c.AdmissionQueueTimeout, "http-admission-queue-timeout", 2*time.Second, "longest wait in the admission queue")
	f.IntVar(&c.MaxInFlightShadow, "http-max-in-flight-shadow-requests", 16, "mirrored shadow attempts in flight; excess mirrors are dropped")
	f.StringVar(&c.OTLPTracesEndpoint, "otlp-traces-endpoint", "", "OTLP/HTTP trace export URL")
	f.StringVar(&c.OTLPHeadersFile, "otlp-headers-file", "", "mounted JSON object of OTLP exporter headers")
	f.Float64Var(&c.TraceSampleRatio, "trace-sample-ratio", 1.0, "trace sampling ratio")
	f.BoolVar(&c.TracePropagateUpstream, "trace-propagate-upstream", true, "inject W3C trace context into provider requests")
	f.BoolVar(&c.TraceAcceptInbound, "trace-accept-inbound", true, "accept inbound W3C trace context")
	f.Int64Var(&c.MaxJSONBodyBytes, "http-max-json-body-bytes", 2097152, "largest JSON request body, before and after gzip inflation")
	f.Int64Var(&c.MaxMediaBodyBytes, "http-max-media-body-bytes", 67108864, "largest raw or multipart media request body")
	f.Int64Var(&c.ProviderMaxResponseBytes, "provider-max-response-bytes", 16777216, "largest buffered provider response body")
	f.Int64Var(&c.ProviderMaxEventBytes, "provider-max-event-bytes", 1048576, "largest single streamed provider event")
	var metricsTenantLabels string
	f.StringVar(&metricsTenantLabels, "metrics-tenant-labels", "", "comma-separated business metric tenant labels: project, key, end_user (OLP_METRICS_TENANT_LABELS)")
	f.IntVar(&c.MetricsSeriesCap, "metrics-series-cap", 5000, "business metric series admitted per process")
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
	// Keys and tokens come only from mounted files, never from the
	// environment, which process listings and diagnostics can expose.
	for _, name := range []string{"OLP_AUTH_HMAC_KEY", "OLP_MASTER_KEY", "OLP_BOOTSTRAP_TOKEN"} {
		if getenv(name) != "" {
			return c, fmt.Errorf("%s is not accepted inline; mount the value and set %s_FILE", name, name)
		}
	}
	for origin := range strings.SplitSeq(corsOrigins, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			c.GatewayCORSAllowedOrigins = append(c.GatewayCORSAllowedOrigins, origin)
		}
	}
	var err error
	if c.DatabaseURL, err = secretURL(c.DatabaseURL, databaseFile, "OLP_DATABASE_URL"); err != nil {
		return c, err
	}
	if c.DatabaseReadURL, err = secretURL(c.DatabaseReadURL, databaseReadFile, "OLP_DATABASE_READ_URL"); err != nil {
		return c, err
	}
	if c.ValkeyURL, err = secretURL(c.ValkeyURL, valkeyFile, "OLP_VALKEY_URL"); err != nil {
		return c, err
	}
	for name, path := range map[string]string{"OLP_AUTH_HMAC_KEY": c.AuthHMACKeyFile, "OLP_BOOTSTRAP_TOKEN": c.BootstrapTokenFile, "OLP_MASTER_KEY": c.MasterKeyFile} {
		if path != "" {
			if _, err := secretURL("", path, name); err != nil {
				// The installation owns whether a retired bootstrap token may be absent.
				if name == "OLP_BOOTSTRAP_TOKEN" && errors.Is(err, os.ErrNotExist) {
					continue
				}
				return c, err
			}
		}
	}
	if c.TrustedProxyCIDRs, err = parseCIDRs("OLP_TRUSTED_PROXY_CIDRS", trustedProxies); err != nil {
		return c, err
	}
	if c.ManagementAllowedCIDRs, err = parseCIDRs("OLP_MANAGEMENT_ALLOWED_CIDRS", managementCIDRs); err != nil {
		return c, err
	}
	if len(c.ManagementAllowedCIDRs) == 0 && strings.TrimSpace(managementCIDRs) != "" {
		return c, errors.New("OLP_MANAGEMENT_ALLOWED_CIDRS must list CIDR prefixes")
	}
	if len(c.ManagementAllowedCIDRs) > 64 {
		return c, errors.New("OLP_MANAGEMENT_ALLOWED_CIDRS accepts at most 64 CIDR ranges")
	}
	for item := range strings.SplitSeq(managementCIDRs, ",") {
		prefix, _ := netip.ParsePrefix(strings.TrimSpace(item))
		if prefix.Addr().Is4In6() {
			return c, errors.New("OLP_MANAGEMENT_ALLOWED_CIDRS must express IPv4-mapped ranges as IPv4")
		}
	}
	if c.ProviderEgressAllowCIDRs, err = parseCIDRs("OLP_PROVIDER_EGRESS_ALLOW_CIDRS", egressCIDRs); err != nil {
		return c, err
	}
	for host := range strings.SplitSeq(egressHosts, ",") {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			c.ProviderEgressAllowHTTPHosts = append(c.ProviderEgressAllowHTTPHosts, host)
		}
	}
	if c.MetricsTenantLabels, err = telemetry.ParseMetricsTenantLabels(metricsTenantLabels); err != nil {
		return c, err
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

func (c Config) Validate() error {
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
	if c.MediaSpoolCapacityBytes < 256<<20 {
		return errors.New("OLP_MEDIA_SPOOL_CAPACITY_BYTES must be at least 256 MiB")
	}
	if c.MaxMediaBodyBytes < 1<<20 || c.MaxMediaBodyBytes > 1024<<20 {
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
