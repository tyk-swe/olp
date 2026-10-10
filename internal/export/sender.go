package export

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"golang.org/x/net/http/httpguts"
	"google.golang.org/protobuf/proto"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

const DeliveryTimeout = 15 * time.Second

const maxCredentialBytes = 65536
const maxDeliveryResponseBytes = 65536

type Destination struct {
	Type         string
	URL          string
	Format       string
	CredentialID *string
}

type Record struct {
	ID     string
	Stream string
	At     time.Time
	Data   json.RawMessage
}

func objectKey(record Record) string {
	if record.Stream == "captures" {
		return CaptureObjectName(record.ID)
	}
	return record.Stream + "/" + record.At.UTC().Format("2006-01-02") + "/" + record.ID + ".jsonl"
}

type senderAuth interface {
	Apply(ctx context.Context, req *http.Request, config connectors.Config, secret, body []byte) (egress.Sensitive, error)
	ApplyAzureStorage(ctx context.Context, req *http.Request, mode string, secret []byte) (egress.Sensitive, error)
}

type Sender struct {
	Installation string
	policy       *egress.Policy
	auth         senderAuth
	client       *http.Client
}

func NewSender(policy *egress.Policy) *Sender {
	client := policy.Client(DeliveryTimeout)
	client.Timeout = DeliveryTimeout
	return &Sender{policy: policy, auth: connectors.NewAuth(policy), client: client}
}

type SendError struct {
	Code string
	Err  error
}

func (e *SendError) Error() string { return e.Code }
func (e *SendError) Unwrap() error { return e.Err }

func sendError(code string, err error) error {
	return &SendError{Code: code, Err: err}
}

func DeliveryErrorCode(err error) string {
	var send *SendError
	if errors.As(err, &send) {
		return send.Code
	}
	return "network"
}

func (s *Sender) Send(ctx context.Context, destination Destination, credential []byte, record Record) error {
	if len(credential) > maxCredentialBytes {
		return sendError("credential", errors.New("credential exceeds 65536 bytes"))
	}
	endpoint, err := s.policy.ValidateEndpoint(destination.URL)
	if err != nil {
		return sendError("endpoint", err)
	}
	switch destination.Type {
	case "https":
		return s.sendHTTPS(ctx, endpoint, credential, record)
	case "otlp_logs":
		return s.sendOTLP(ctx, endpoint, credential, record)
	case "s3", "gcs", "azure_blob":
		return s.sendObject(ctx, endpoint, destination, credential, record)
	default:
		return sendError("endpoint", fmt.Errorf("unknown sink type %q", destination.Type))
	}
}

func (s *Sender) do(req *http.Request) error {
	response, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return sendError("timeout", err)
		}
		return sendError("network", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDeliveryResponseBytes))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return sendError("status", fmt.Errorf("destination answered %d", response.StatusCode))
	}
	return nil
}

type credentialDocument struct {
	SigningSecret string            `json:"signing_secret"`
	Headers       map[string]string `json:"headers"`
	Region        string            `json:"region"`
	AuthMode      string            `json:"auth_mode"`
	Secret        json.RawMessage   `json:"secret"`
}

func parseCredential(raw []byte) (credentialDocument, error) {
	var document credentialDocument
	if len(raw) == 0 {
		return document, nil
	}
	if len(raw) > maxCredentialBytes {
		return document, errors.New("credential exceeds 65536 bytes")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&document); err != nil {
		return document, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return document, errors.New("credential must be one JSON document")
	}
	return document, nil
}

var reservedCredentialHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true, "trailers": true,
	"transfer-encoding": true, "upgrade": true, "host": true,
	"content-length": true, "x-olp-signature": true, "x-olp-event-id": true,
}

func applyCredentialHeaders(header http.Header, headers map[string]string) error {
	for name, value := range headers {
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) || reservedCredentialHeaders[strings.ToLower(name)] {
			return errors.New("credential headers carry a forbidden name or value")
		}
	}
	for name, value := range headers {
		header.Set(name, value)
	}
	return nil
}

func (s *Sender) sendHTTPS(ctx context.Context, endpoint *url.URL, credential []byte, record Record) error {
	credentialDoc, err := parseCredential(credential)
	if err != nil {
		return sendError("credential", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(record.Data))
	if err != nil {
		return sendError("network", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-OLP-Event-ID", record.ID)
	if credentialDoc.SigningSecret != "" {
		if len(credentialDoc.SigningSecret) < 16 {
			return sendError("credential", errors.New("signing_secret must be at least 16 bytes"))
		}
		if strings.ContainsAny(credentialDoc.SigningSecret, "\r\n") {
			return sendError("credential", errors.New("signing_secret must not contain newlines"))
		}
		sum := hmac.New(sha256.New, []byte(credentialDoc.SigningSecret))
		sum.Write(record.Data)
		request.Header.Set("X-OLP-Signature", "sha256="+hex.EncodeToString(sum.Sum(nil)))
	}
	if err := applyCredentialHeaders(request.Header, credentialDoc.Headers); err != nil {
		return sendError("credential", err)
	}
	return s.do(request)
}

func (s *Sender) sendOTLP(ctx context.Context, endpoint *url.URL, credential []byte, record Record) error {
	credentialDoc, err := parseCredential(credential)
	if err != nil {
		return sendError("credential", err)
	}
	attributes := []*commonpb.KeyValue{
		{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "openllmproxy"}}},
	}
	if s.Installation != "" {
		attributes = append(attributes, &commonpb.KeyValue{Key: "olp.installation_id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s.Installation}}})
	}
	logRecord := &logspb.LogRecord{
		TimeUnixNano: uint64(record.At.UnixNano()),
		Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: string(record.Data)}},
		Attributes: []*commonpb.KeyValue{
			{Key: "olp.event_id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: record.ID}}},
			{Key: "olp.stream", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: record.Stream}}},
		},
	}
	exportRequest := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: attributes},
		ScopeLogs: []*logspb.ScopeLogs{{
			LogRecords: []*logspb.LogRecord{logRecord},
		}},
	}}}
	payload, err := proto.Marshal(exportRequest)
	if err != nil {
		return sendError("endpoint", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return sendError("network", err)
	}
	request.Header.Set("Content-Type", "application/x-protobuf")
	if err := applyCredentialHeaders(request.Header, credentialDoc.Headers); err != nil {
		return sendError("credential", err)
	}
	return s.do(request)
}

func (s *Sender) sendObject(ctx context.Context, endpoint *url.URL, destination Destination, credential []byte, record Record) error {
	if destination.Format != "jsonl" {
		return sendError("endpoint", errors.New("object sinks export jsonl only"))
	}
	credentialDoc, err := parseCredential(credential)
	if err != nil {
		return sendError("credential", err)
	}
	object := *endpoint
	object.Path = strings.TrimRight(endpoint.Path, "/") + "/" + objectKey(record)
	body := append(append([]byte{}, record.Data...), '\n')
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, object.String(), bytes.NewReader(body))
	if err != nil {
		return sendError("network", err)
	}
	request.Header.Set("Content-Type", "application/x-ndjson")
	switch destination.Type {
	case "s3":
		region := credentialDoc.Region
		if region == "" {
			region = s3EndpointRegion(endpoint)
		}
		if region == "" {
			region = os.Getenv("AWS_REGION")
		}
		if region == "" {
			region = os.Getenv("AWS_DEFAULT_REGION")
		}
		if region == "" {
			return sendError("credential", errors.New("s3 requires a region in the credential, the endpoint or the workload environment"))
		}
		mode := credentialDoc.AuthMode
		if mode == "" {
			mode = "default_chain"
		}
		if mode != "static" && mode != "default_chain" {
			return sendError("credential", errors.New("s3 auth_mode must be static or default_chain"))
		}
		if mode == "static" && len(credentialDoc.Secret) == 0 {
			return sendError("credential", errors.New("s3 static auth_mode requires a secret"))
		}
		if _, err := s.auth.Apply(ctx, request, connectors.Config{Kind: "bedrock", SigningService: "s3", CloudRegion: region, AuthMode: mode}, credentialDoc.Secret, body); err != nil {
			if errors.Is(err, connectors.ErrCredentialRejected) {
				return sendError("credential", err)
			}
			return sendError("authentication", err)
		}
	case "gcs":
		mode := credentialDoc.AuthMode
		if mode == "" {
			mode = "adc"
		}
		if mode != "service_account" && mode != "adc" {
			return sendError("credential", errors.New("gcs auth_mode must be service_account or adc"))
		}
		if mode == "service_account" && len(credentialDoc.Secret) == 0 {
			return sendError("credential", errors.New("gcs service_account auth_mode requires a secret"))
		}
		if _, err := s.auth.Apply(ctx, request, connectors.Config{Kind: "vertex_ai", AuthMode: mode}, credentialDoc.Secret, body); err != nil {
			if errors.Is(err, connectors.ErrCredentialRejected) {
				return sendError("credential", err)
			}
			return sendError("authentication", err)
		}
	case "azure_blob":
		mode := credentialDoc.AuthMode
		if mode == "" {
			mode = "azure_default"
		}
		if mode != "azure_client_secret" && mode != "azure_default" {
			return sendError("credential", errors.New("azure_blob auth_mode must be azure_client_secret or azure_default"))
		}
		if mode == "azure_client_secret" && len(credentialDoc.Secret) == 0 {
			return sendError("credential", errors.New("azure_blob azure_client_secret auth_mode requires a secret"))
		}
		request.Header.Set("X-Ms-Blob-Type", "BlockBlob")
		request.Header.Set("X-Ms-Version", "2023-11-03")
		request.Header.Set("X-Ms-Date", time.Now().UTC().Format(http.TimeFormat))
		if _, err := s.auth.ApplyAzureStorage(ctx, request, mode, credentialDoc.Secret); err != nil {
			if errors.Is(err, connectors.ErrCredentialRejected) {
				return sendError("credential", err)
			}
			return sendError("authentication", err)
		}
	}
	return s.do(request)
}

func s3EndpointRegion(endpoint *url.URL) string {
	host := endpoint.Hostname()
	for _, marker := range []string{".s3.", ".s3-"} {
		if i := strings.Index(host, marker); i >= 0 {
			rest := host[i+len(marker):]
			if j := strings.IndexByte(rest, '.'); j > 0 {
				return rest[:j]
			}
		}
	}
	return ""
}
