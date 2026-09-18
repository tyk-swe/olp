// Package telemetry owns optional OTLP/HTTP trace export: exporter
// construction, endpoint and secret validation, bounded batching, and the
// allowlisted request and attempt spans the gateway records. Tracing is
// installed only when an endpoint is configured; everything downstream gates
// on a non-nil runtime rather than re-deriving "tracing is on" from a flag.
package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	exportTimeout       = 2 * time.Second
	exportQueueCapacity = 2048
	exportBatchSize     = 256
	exportScheduleDelay = 200 * time.Millisecond
)

// Ambient OTLP header variables are rejected because exporter credentials may
// live only in the dedicated secret file.
var ambientOTLPHeaderVariables = []string{
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	"OTEL_EXPORTER_OTLP_HEADERS",
}

var exportDroppedTotal atomic.Uint64

// Config is the validated startup configuration for trace export.
type Config struct {
	Endpoint          string
	HeadersFile       string
	SampleRatio       float64
	PropagateUpstream bool
	AcceptInbound     bool
	Mode              string
	Version           string
}

// RuntimeConfig is the per-request policy while an exporter is installed.
type RuntimeConfig struct {
	PropagateUpstream bool
	AcceptInbound     bool
}

// ForInstallation binds the installation id recorded on every request span.
func (r RuntimeConfig) ForInstallation(installationID string) RequestConfig {
	return RequestConfig{
		InstallationID:    installationID,
		PropagateUpstream: r.PropagateUpstream,
		AcceptInbound:     r.AcceptInbound,
	}
}

// RequestConfig is the request-path configuration while tracing is enabled.
type RequestConfig struct {
	InstallationID    string
	PropagateUpstream bool
	AcceptInbound     bool
}

// Handle is the installed telemetry plane: a provider when an endpoint is
// configured, or only the runtime toggles when it is not.
type Handle struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	runtime  RuntimeConfig
}

// Install validates the configuration and, when an endpoint is set, builds the
// exporter and provider. An unset endpoint installs no exporter at all.
func Install(cfg Config) (*Handle, error) {
	if cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return nil, errors.New("OLP_TRACE_SAMPLE_RATIO must be between 0.0 and 1.0")
	}
	h := &Handle{tracer: noop.NewTracerProvider().Tracer("openllmproxy"), runtime: RuntimeConfig{
		PropagateUpstream: cfg.PropagateUpstream,
		AcceptInbound:     cfg.AcceptInbound,
	}}
	if cfg.Endpoint == "" {
		return h, nil
	}
	if err := ValidateEndpoint(cfg.Endpoint); err != nil {
		return nil, err
	}
	if err := RejectAmbientHeaders(func(name string) bool { _, ok := os.LookupEnv(name); return ok }); err != nil {
		return nil, err
	}
	headers, err := LoadHeaders(cfg.HeadersFile)
	if err != nil {
		return nil, err
	}
	exporter, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
		otlptracehttp.WithTimeout(exportTimeout),
		otlptracehttp.WithHeaders(headers),
	)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	h.provider = sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(NewBoundedSpanProcessor(
			&countingExporter{inner: exporter},
			exportQueueCapacity, exportBatchSize, exportScheduleDelay,
		)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", "openllmproxy"),
			attribute.String("service.version", version),
			attribute.String("olp.process.mode", cfg.Mode),
		)),
	)
	h.tracer = h.provider.Tracer("openllmproxy")
	return h, nil
}

// Runtime is the per-request configuration, or nil when no exporter is
// installed. Callers gate on this rather than re-deriving "tracing is on".
func (h *Handle) Runtime() *RuntimeConfig {
	if h == nil || h.provider == nil {
		return nil
	}
	runtime := h.runtime
	return &runtime
}

// Tracer returns the process tracer. When no exporter is installed it is a
// no-op tracer, so instrumented call sites never branch on nil.
func (h *Handle) Tracer() trace.Tracer {
	if h == nil {
		return noop.NewTracerProvider().Tracer("openllmproxy")
	}
	return h.tracer
}

// Shutdown flushes and stops the exporter. It is a no-op when tracing was
// never installed.
func (h *Handle) Shutdown(ctx context.Context) error {
	if h == nil || h.provider == nil {
		return nil
	}
	if err := h.provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("trace exporter did not flush cleanly during shutdown: %w", err)
	}
	return nil
}

// ValidateEndpoint requires an HTTP(S) URL with a host and no credentials or
// fragment. The value is used without path rewriting.
func ValidateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("OLP_OTLP_TRACES_ENDPOINT must be a valid URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return errors.New("OLP_OTLP_TRACES_ENDPOINT must be an HTTP or HTTPS URL with a host")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return errors.New("OLP_OTLP_TRACES_ENDPOINT must not contain credentials or a fragment")
	}
	return nil
}

// RejectAmbientHeaders refuses the ambient OTLP header variables: exporter
// credentials belong in OLP_OTLP_HEADERS_FILE, never in the process
// environment where they are visible to every child and crash dump.
func RejectAmbientHeaders(set func(string) bool) error {
	for _, name := range ambientOTLPHeaderVariables {
		if set(name) {
			return fmt.Errorf("%s is not supported; use OLP_OTLP_HEADERS_FILE", name)
		}
	}
	return nil
}

// LoadHeaders reads the JSON object of exporter headers. The file must carry
// secret-file permissions, and every entry must be a legal HTTP header pair.
func LoadHeaders(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := secrets.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("OLP_OTLP_HEADERS_FILE: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, errors.New("OLP_OTLP_HEADERS_FILE must contain a JSON object of string values")
	}
	for name, value := range values {
		if !validHeaderName(name) {
			return nil, errors.New("OLP_OTLP_HEADERS_FILE contains an invalid header name")
		}
		if !validHeaderValue(value) {
			return nil, errors.New("OLP_OTLP_HEADERS_FILE contains an invalid header value")
		}
	}
	return values, nil
}

func validHeaderName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '!' || c == '#' || c == '$' || c == '%' || c == '&' || c == '\'' ||
			c == '*' || c == '+' || c == '-' || c == '.' || c == '^' || c == '_' ||
			c == '`' || c == '|' || c == '~'
		if !ok {
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < 0x20 && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

// ExportDroppedTotal is the number of spans dropped before a successful OTLP
// export, whether by a full queue or a failed batch.
func ExportDroppedTotal() uint64 { return exportDroppedTotal.Load() }

func recordExportDrops(count uint64) { exportDroppedTotal.Add(count) }

// countingExporter counts every span in a failed batch as dropped.
type countingExporter struct{ inner sdktrace.SpanExporter }

func (c *countingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := c.inner.ExportSpans(ctx, spans)
	if err != nil {
		recordExportDrops(uint64(len(spans)))
	}
	return err
}

func (c *countingExporter) Shutdown(ctx context.Context) error { return c.inner.Shutdown(ctx) }

// Inject writes the current W3C trace context into outbound headers when
// upstream propagation is enabled for this request.
func Inject(ctx context.Context, header http.Header, propagate bool) {
	if !propagate {
		return
	}
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(header))
}
