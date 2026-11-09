package destination

import (
	"context"
	"encoding/json"
	"io"
	"net/url"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tyk-swe/olp/internal/management/contract"
)

type OTLPLogs struct{}

func (OTLPLogs) Type() Type     { return contract.CreateExportSinkRequestTypeOtlpLogs }
func (OTLPLogs) Format() Format { return contract.CreateExportSinkRequestFormatOtlp }

func parseOTLPLogs(_ Type, settings json.RawMessage) (Spec, error) {
	return OTLPLogs{}, decodeSettings(contract.CreateExportSinkRequestFormatOtlp, settings, &struct{}{})
}

type otlpLogs struct{ headers map[string]string }

func (OTLPLogs) transport(raw []byte, strict bool) (transport, error) {
	var credential struct {
		Headers map[string]string `json:"headers"`
	}
	if err := decodeCredential(raw, strict, &credential, "otlp_logs credentials carry only headers"); err != nil {
		return nil, err
	}
	return otlpLogs{headers: credential.Headers}, checkHeaders(credential.Headers)
}

func text(value string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
}

func (o otlpLogs) send(ctx context.Context, s *Sender, _ Target, endpoint *url.URL, record Record) error {
	attributes := []*commonpb.KeyValue{{Key: "service.name", Value: text("openllmproxy")}}
	if s.installation != "" {
		attributes = append(attributes, &commonpb.KeyValue{Key: "olp.installation_id", Value: text(s.installation)})
	}
	payload, err := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: attributes},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: uint64(record.At.UnixNano()),
			Body:         text(string(record.Data)),
			Attributes: []*commonpb.KeyValue{
				{Key: "olp.event_id", Value: text(record.ID)},
				{Key: "olp.stream", Value: text(record.Stream)},
			},
		}}}},
	}}})
	if err != nil {
		return sendError("endpoint", err)
	}
	request, err := post(ctx, endpoint, "application/x-protobuf", payload)
	if err != nil {
		return err
	}
	for name, value := range o.headers {
		request.Header.Set(name, value)
	}
	response, err := s.response(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDeliveryResponseBytes+1))
	if err != nil || len(body) > maxDeliveryResponseBytes {
		return sendError("otlp_response", nil)
	}
	var result collogspb.ExportLogsServiceResponse
	if proto.Unmarshal(body, &result) != nil {
		return sendError("otlp_response", nil)
	}
	if result.GetPartialSuccess().GetRejectedLogRecords() > 0 {
		return &SendError{Code: "otlp_rejected", Rejected: true}
	}
	return nil
}
