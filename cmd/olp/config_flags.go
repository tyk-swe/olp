package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/staging"
)

type configFlagValues struct{ databaseFile, databasePasswordFile, databasePasswordFileURL, databaseReadFile, valkeyFile, cacheValkeyFile, level, trustedProxies, managementCIDRs, egressCIDRs, egressHosts, corsOrigins, metricsTenantLabels string }

func registerConfigFlags(f *flag.FlagSet, c *Config) *configFlagValues {
	v := new(configFlagValues)
	f.StringVar(&c.DatabaseURL, "database-url", "", "PostgreSQL URL (OLP_DATABASE_URL)")
	f.StringVar(&v.databaseFile, "database-url-file", "", "file containing PostgreSQL URL")
	f.StringVar(&v.databasePasswordFile, "database-password-file", "", "mounted PostgreSQL password when the URL provides none")
	f.StringVar(&v.databasePasswordFileURL, "database-password-file-url", "", "apply the mounted PostgreSQL password only to this PostgreSQL endpoint")
	f.StringVar(&c.DatabaseReadURL, "database-read-url", "", "optional PostgreSQL replica for runtime release and authority reads")
	f.StringVar(&c.Region, "region", "", "deployment region for regional limits and local provider preference")
	f.StringVar(&v.databaseReadFile, "database-read-url-file", "", "file containing optional PostgreSQL read-replica URL")
	f.IntVar(&c.DatabaseMaxConnections, "database-max-connections", 20, "PostgreSQL pool capacity")
	f.StringVar(&c.CacheValkeyURL, "cache-valkey-url", "", "optional dedicated response cache Valkey URL")
	f.StringVar(&v.cacheValkeyFile, "cache-valkey-url-file", "", "file containing dedicated response cache Valkey URL")
	f.StringVar(&c.CacheValkeyCAFile, "cache-valkey-tls-ca-file", "", "PEM trust roots for response cache Valkey")
	f.StringVar(&c.ValkeyURL, "valkey-url", "", "redis:// or rediss:// URL; required for worker")
	f.StringVar(&v.valkeyFile, "valkey-url-file", "", "file containing Valkey URL")
	f.StringVar(&c.ValkeyCAFile, "valkey-tls-ca-file", "", "PEM trust roots for Valkey TLS")
	f.StringVar(&c.ListenAddr, "listen-addr", "127.0.0.1:8080", "public listener")
	f.StringVar(&c.ObservabilityListenAddr, "observability-listen-addr", "127.0.0.1:9090", "private health listener")
	f.StringVar(&c.PublicOrigin, "public-origin", "http://127.0.0.1:8080", "browser origin")
	f.StringVar(&c.ConsoleDir, "console-dir", "console/build", "static console directory")
	f.StringVar(&c.MediaSpoolDir, "media-spool-dir", "", "directory for bounded media staging; defaults to the system temp directory")
	f.Int64Var(&c.MediaSpoolCapacityBytes, "media-spool-capacity-bytes", staging.DefaultCapacityBytes, "media spool capacity in bytes")
	f.StringVar(&c.AuthHMACKeyFile, "auth-hmac-key-file", "", "mounted hex/base64 authentication key")
	f.StringVar(&c.BootstrapTokenFile, "bootstrap-token-file", "", "mounted first-owner bootstrap token")
	f.StringVar(&c.ConnectorConfigFile, "connector-config-file", "", "mounted provider transport configuration")
	f.StringVar(&c.UnconfinedPluginDir, "unconfined-plugin-dir", "", "absolute directory of unconfined plugin executables; setting it enables the experimental unconfined plugin tier")
	f.StringVar(&c.MasterKeyFile, "master-key-file", "", "mounted JSON master key ring")
	f.StringVar(&c.RuntimeDatabaseRole, "runtime-role", "", "existing runtime role granted access by migrate")
	f.StringVar(&v.level, "log-level", "info", "debug, info, warn, or error (OLP_LOG_LEVEL)")
	f.DurationVar(&c.RequestTimeout, "dependency-request-timeout", 2*time.Second, "dependency request deadline")
	f.DurationVar(&c.StartupTimeout, "startup-timeout", 10*time.Second, "startup deadline")
	f.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", 30*time.Second, "total shutdown deadline")
	f.BoolVar(&c.LocalLoginEnabled, "local-login-enabled", true, "allow password sign-in")
	f.StringVar(&v.corsOrigins, "gateway-cors-allowed-origins", "", "comma-separated origins allowed to call the inference API")
	f.IntVar(&c.MaxInlineMediaItems, "http-max-inline-media-items", 4, "maximum inline media items")
	f.Int64Var(&c.MaxInlineMediaItemBytes, "http-max-inline-media-item-bytes", 1048576, "maximum decoded bytes per inline media item")
	f.Int64Var(&c.MaxInlineMediaTotalBytes, "http-max-inline-media-total-bytes", 2097152, "maximum decoded inline media bytes per request")
	f.StringVar(&v.trustedProxies, "trusted-proxy-cidrs", "", "comma-separated CIDRs allowed to supply X-Forwarded-For")
	f.StringVar(&v.managementCIDRs, "management-allowed-cidrs", "", "comma-separated client CIDRs allowed to reach management and console; empty allows all")
	f.StringVar(&v.egressCIDRs, "provider-egress-allow-cidrs", "", "comma-separated CIDRs exempt from the non-public provider egress denylist")
	f.StringVar(&v.egressHosts, "provider-egress-allow-http-hosts", "", "comma-separated hosts whose provider endpoints may use plain HTTP")
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
	f.StringVar(&v.metricsTenantLabels, "metrics-tenant-labels", "", "comma-separated business metric tenant labels: project, key, end_user (OLP_METRICS_TENANT_LABELS)")
	f.IntVar(&c.MetricsSeriesCap, "metrics-series-cap", 5000, "business metric series admitted per process")
	return v
}

func (v *configFlagValues) resolve(c Config, getenv func(string) string) (Config, error) {
	// Keys and tokens come only from mounted files, never from the
	// environment, which process listings and diagnostics can expose.
	for _, name := range []string{"OLP_AUTH_HMAC_KEY", "OLP_MASTER_KEY", "OLP_BOOTSTRAP_TOKEN"} {
		if getenv(name) != "" {
			return c, fmt.Errorf("%s is not accepted inline; mount the value and set %s_FILE", name, name)
		}
	}
	for origin := range strings.SplitSeq(v.corsOrigins, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			c.GatewayCORSAllowedOrigins = append(c.GatewayCORSAllowedOrigins, origin)
		}
	}
	var err error
	if c.DatabaseURL, err = secretURL(c.DatabaseURL, v.databaseFile, "OLP_DATABASE_URL"); err != nil {
		return c, err
	}
	if v.databasePasswordFile != "" && getenv("PGPASSWORD") == "" {
		if c.DatabaseURL, err = applyDatabasePassword(c.DatabaseURL, v.databasePasswordFile, v.databasePasswordFileURL); err != nil {
			return c, err
		}
	}
	if c.DatabaseReadURL, err = secretURL(c.DatabaseReadURL, v.databaseReadFile, "OLP_DATABASE_READ_URL"); err != nil {
		return c, err
	}
	if c.CacheValkeyURL, err = secretURL(c.CacheValkeyURL, v.cacheValkeyFile, "OLP_CACHE_VALKEY_URL"); err != nil {
		return c, err
	}
	if c.ValkeyURL, err = secretURL(c.ValkeyURL, v.valkeyFile, "OLP_VALKEY_URL"); err != nil {
		return c, err
	}
	for name, path := range map[string]string{"OLP_AUTH_HMAC_KEY": c.AuthHMACKeyFile, "OLP_MASTER_KEY": c.MasterKeyFile} {
		if path != "" {
			if _, err := secretURL("", path, name); err != nil {
				return c, err
			}
		}
	}
	if c.TrustedProxyCIDRs, err = parseCIDRs("OLP_TRUSTED_PROXY_CIDRS", v.trustedProxies); err != nil {
		return c, err
	}
	if c.ManagementAllowedCIDRs, err = parseCIDRs("OLP_MANAGEMENT_ALLOWED_CIDRS", v.managementCIDRs); err != nil {
		return c, err
	}
	if len(c.ManagementAllowedCIDRs) == 0 && strings.TrimSpace(v.managementCIDRs) != "" {
		return c, errors.New("OLP_MANAGEMENT_ALLOWED_CIDRS must list CIDR prefixes")
	}
	if len(c.ManagementAllowedCIDRs) > 64 {
		return c, errors.New("OLP_MANAGEMENT_ALLOWED_CIDRS accepts at most 64 CIDR ranges")
	}
	for item := range strings.SplitSeq(v.managementCIDRs, ",") {
		prefix, _ := netip.ParsePrefix(strings.TrimSpace(item))
		if prefix.Addr().Is4In6() {
			return c, errors.New("OLP_MANAGEMENT_ALLOWED_CIDRS must express IPv4-mapped ranges as IPv4")
		}
	}
	if c.ProviderEgressAllowCIDRs, err = parseCIDRs("OLP_PROVIDER_EGRESS_ALLOW_CIDRS", v.egressCIDRs); err != nil {
		return c, err
	}
	for host := range strings.SplitSeq(v.egressHosts, ",") {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			c.ProviderEgressAllowHTTPHosts = append(c.ProviderEgressAllowHTTPHosts, host)
		}
	}
	if c.MetricsTenantLabels, err = telemetry.ParseMetricsTenantLabels(v.metricsTenantLabels); err != nil {
		return c, fmt.Errorf("OLP_METRICS_TENANT_LABELS %w", err)
	}
	if err = c.LogLevel.UnmarshalText([]byte(v.level)); err != nil {
		return c, errors.New("OLP_LOG_LEVEL must be debug, info, warn, or error")
	}
	if c.LogLevel != slog.LevelDebug && c.LogLevel != slog.LevelInfo && c.LogLevel != slog.LevelWarn && c.LogLevel != slog.LevelError {
		return c, errors.New("OLP_LOG_LEVEL must be debug, info, warn, or error")
	}
	return c, c.Validate()
}
