package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	exportTimeout       = 2 * time.Second
	exportQueueCapacity = 2048
	exportBatchSize     = 256
	exportScheduleDelay = 200 * time.Millisecond
)

var ambientOTLPHeaderVariables = []string{
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	"OTEL_EXPORTER_OTLP_HEADERS",
}

type Propagation struct {
	Upstream      bool
	AcceptInbound bool
}

type Config struct {
	Endpoint    string
	HeadersFile string
	SampleRatio float64
	Propagation Propagation
	Mode        string
	Version     string
}

type Handle struct {
	provider    *sdktrace.TracerProvider
	propagation Propagation
	dropped     prometheus.Counter
}

func Install(cfg Config, lookupEnv func(string) (string, bool)) (*Handle, error) {
	dropped := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "olp_trace_export_dropped_total",
		Help: "Spans dropped before successful OTLP export.",
	})
	if cfg.Endpoint == "" {
		return &Handle{dropped: dropped}, nil
	}
	if err := validateEndpoint(cfg.Endpoint); err != nil {
		return nil, err
	}
	if err := rejectAmbientHeaders(lookupEnv); err != nil {
		return nil, err
	}
	headers, err := loadHeaders(cfg.HeadersFile)
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
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(newBoundedSpanProcessor(&countingExporter{inner: exporter, dropped: dropped}, dropped)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", "openllmproxy"),
			attribute.String("service.version", version),
			attribute.String("olp.process.mode", cfg.Mode),
		)),
	)
	return &Handle{provider: provider, propagation: cfg.Propagation, dropped: dropped}, nil
}

func (h *Handle) Middleware(installationID string) func(http.Handler) http.Handler {
	if h.provider == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return admittedRequest(tracerFrom(h.provider), installationID, h.propagation, next)
	}
}

func (h *Handle) Metrics() prometheus.Collector { return h.dropped }

func (h *Handle) Shutdown(ctx context.Context) error {
	if h.provider == nil {
		return nil
	}
	if err := h.provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("trace exporter did not flush cleanly during shutdown: %w", err)
	}
	return nil
}

func validateEndpoint(endpoint string) error {
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

// Exporter credentials belong only in OLP_OTLP_HEADERS_FILE, never in the
// process environment where every child and crash dump can see them.
func rejectAmbientHeaders(lookupEnv func(string) (string, bool)) error {
	for _, name := range ambientOTLPHeaderVariables {
		if _, set := lookupEnv(name); set {
			return fmt.Errorf("%s is not supported; use OLP_OTLP_HEADERS_FILE", name)
		}
	}
	return nil
}

func loadHeaders(path string) (map[string]string, error) {
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
		if !httpguts.ValidHeaderFieldName(name) {
			return nil, errors.New("OLP_OTLP_HEADERS_FILE contains an invalid header name")
		}
		if !httpguts.ValidHeaderFieldValue(value) {
			return nil, errors.New("OLP_OTLP_HEADERS_FILE contains an invalid header value")
		}
	}
	return values, nil
}
