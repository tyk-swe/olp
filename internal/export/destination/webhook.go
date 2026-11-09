package destination

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type JSONWebhook struct{}

type CloudEvents struct {
	Source string `json:"source,omitempty"`
}

func (JSONWebhook) Type() Type     { return contract.CreateExportSinkRequestTypeHttps }
func (JSONWebhook) Format() Format { return contract.CreateExportSinkRequestFormatJson }
func (CloudEvents) Type() Type     { return contract.CreateExportSinkRequestTypeHttps }
func (CloudEvents) Format() Format { return contract.CreateExportSinkRequestFormatCloudevents }

func parseJSONWebhook(_ Type, settings json.RawMessage) (Spec, error) {
	return JSONWebhook{}, decodeSettings(contract.CreateExportSinkRequestFormatJson, settings, &struct{}{})
}

func parseCloudEvents(_ Type, settings json.RawMessage) (Spec, error) {
	var input struct {
		Source *string `json:"source"`
	}
	if err := decodeSettings(contract.CreateExportSinkRequestFormatCloudevents, settings, &input, "source"); err != nil {
		return nil, err
	}
	if input.Source == nil {
		return CloudEvents{}, nil
	}
	source := *input.Source
	_, parseErr := url.Parse(source)
	if len(source) < 1 || len(source) > 256 || parseErr != nil || strings.IndexFunc(source, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) >= 0 {
		return nil, access.Invalid("settings.source", "source must be a URI reference of 1 to 256 characters without whitespace")
	}
	return CloudEvents{Source: source}, nil
}

type webhookCredential struct {
	SigningSecret string            `json:"signing_secret"`
	Headers       map[string]string `json:"headers"`
}

func parseWebhookCredential(raw []byte, strict bool) (webhookCredential, error) {
	var credential webhookCredential
	if err := decodeCredential(raw, strict, &credential, "https credentials carry only signing_secret and headers"); err != nil {
		return credential, err
	}
	if err := checkSigningSecret(credential.SigningSecret); err != nil {
		return credential, err
	}
	return credential, checkHeaders(credential.Headers)
}

func checkSigningSecret(secret string) error {
	switch {
	case secret == "":
		return nil
	case len(secret) < 16:
		return errors.New("signing_secret must be at least 16 bytes")
	case strings.ContainsAny(secret, "\r\n"):
		return errors.New("signing_secret must not contain newlines")
	}
	return nil
}

var reservedHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true, "trailers": true,
	"transfer-encoding": true, "upgrade": true, "host": true,
	"content-length": true, "x-olp-signature": true, "x-olp-event-id": true,
}

func checkHeaders(headers map[string]string) error {
	for name, value := range headers {
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) || reservedHeaders[strings.ToLower(name)] {
			return errors.New("credential headers carry a forbidden name or value")
		}
	}
	return nil
}

func (c webhookCredential) apply(header http.Header, body []byte) {
	if c.SigningSecret != "" {
		egress.SignBody(header, []byte(c.SigningSecret), body)
	}
	for name, value := range c.Headers {
		header.Set(name, value)
	}
}

type jsonWebhook struct{ credential webhookCredential }

type cloudEventsWebhook struct {
	spec       CloudEvents
	credential webhookCredential
}

func (JSONWebhook) transport(raw []byte, strict bool) (transport, error) {
	credential, err := parseWebhookCredential(raw, strict)
	return jsonWebhook{credential: credential}, err
}

func (c CloudEvents) transport(raw []byte, strict bool) (transport, error) {
	credential, err := parseWebhookCredential(raw, strict)
	return cloudEventsWebhook{spec: c, credential: credential}, err
}

func (w jsonWebhook) send(ctx context.Context, s *Sender, _ Target, endpoint *url.URL, record Record) error {
	request, err := post(ctx, endpoint, "application/json", record.Data)
	if err != nil {
		return err
	}
	request.Header.Set("X-OLP-Event-ID", record.ID)
	w.credential.apply(request.Header, record.Data)
	return s.do(request)
}

type cloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
}

func (w cloudEventsWebhook) send(ctx context.Context, s *Sender, _ Target, endpoint *url.URL, record Record) error {
	envelope, err := readEnvelope(record)
	if err != nil {
		return sendError("endpoint", err)
	}
	source := w.spec.Source
	if source == "" {
		source = "/olp/installations/" + s.installation
	}
	subject := "installation"
	if envelope.ProjectID != nil && *envelope.ProjectID != "" {
		subject = *envelope.ProjectID
	}
	body, err := json.Marshal(cloudEvent{
		SpecVersion:     "1.0",
		ID:              envelope.SourceID,
		Source:          source,
		Type:            "dev.openllmproxy.attempt.priced",
		Subject:         subject,
		Time:            envelope.OccurredAt.Format(time.RFC3339Nano),
		DataContentType: "application/json",
		Data:            envelope.Data,
	})
	if err != nil {
		return sendError("endpoint", err)
	}
	request, err := post(ctx, endpoint, "application/cloudevents+json", body)
	if err != nil {
		return err
	}
	request.Header.Set("X-OLP-Event-ID", record.ID)
	w.credential.apply(request.Header, body)
	return s.do(request)
}

type envelope struct {
	SourceID   string
	ProjectID  *string
	OccurredAt time.Time
	Data       json.RawMessage
}

func readEnvelope(record Record) (envelope, error) {
	var raw struct {
		SourceID   string          `json:"source_id"`
		ProjectID  *string         `json:"project_id"`
		OccurredAt string          `json:"occurred_at"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(record.Data, &raw); err != nil {
		return envelope{}, err
	}
	if raw.SourceID == "" {
		return envelope{}, errors.New("record envelope has no source_id")
	}
	at, err := time.Parse(time.RFC3339Nano, raw.OccurredAt)
	if err != nil {
		return envelope{}, err
	}
	return envelope{SourceID: raw.SourceID, ProjectID: raw.ProjectID, OccurredAt: at.UTC(), Data: raw.Data}, nil
}
