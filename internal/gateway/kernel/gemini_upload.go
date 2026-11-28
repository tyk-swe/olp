package kernel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
)

func GeminiUploadOrigin(cfg connectors.Config, model string, egressPolicy interface {
	ValidateEndpoint(string) (*url.URL, error)
}) (string, error) {
	generate, err := cfg.URL(wire.Gemini, model, false)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(generate)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("Gemini GenerateContent endpoint is malformed")
	}
	suffix := "/models/" + url.PathEscape(cfg.Model(model)) + ":generateContent"
	if !strings.HasSuffix(parsed.EscapedPath(), suffix) {
		return "", errors.New("Gemini GenerateContent path is not recognized")
	}
	origin := parsed.Scheme + "://" + parsed.Host
	endpoint := origin + "/upload/v1beta/files"
	if _, err := egressPolicy.ValidateEndpoint(endpoint); err != nil {
		return "", err
	}
	return endpoint, nil
}

func ValidateGeminiUploadURL(uploadURL, providerEndpoint string, egressPolicy interface {
	ValidateEndpoint(string) (*url.URL, error)
}) error {
	upload, err := url.Parse(uploadURL)
	if err != nil || upload.Scheme != "https" || upload.Host == "" || upload.User != nil || upload.Fragment != "" {
		return errors.New("Gemini resumable upload URL must be an absolute HTTPS URL")
	}
	provider, err := url.Parse(providerEndpoint)
	if err != nil || !strings.EqualFold(upload.Host, provider.Host) {
		return errors.New("Gemini resumable upload URL host differs from the provider")
	}
	validationURL := *upload
	validationURL.RawQuery, validationURL.ForceQuery = "", false
	if _, err := egressPolicy.ValidateEndpoint(validationURL.String()); err != nil {
		return errors.New("Gemini resumable upload URL is outside egress policy")
	}
	return nil
}

func (s *Server) GeminiPinnedRequest(ctx context.Context, x *Call, p *Pin, method, endpoint string, body io.Reader, length int64, contentType, accept string, headers http.Header) (*PinnedResponse, *AttemptFailure) {
	exchange := s.beginPinned(ctx, x, p, method)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil {
		return nil, exchange.fail(ClassConnect, nil)
	}
	validationURL := *parsed
	validationURL.RawQuery, validationURL.ForceQuery = "", false
	if _, err := s.Egress.ValidateEndpoint(validationURL.String()); err != nil {
		return nil, exchange.fail(ClassConnect, nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return nil, exchange.fail(ClassConnect, nil)
	}
	req.Header.Set("User-Agent", "olp/gateway")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if body != nil {
		req.ContentLength = length
	}
	if err := s.applySlotCredential(ctx, x, req, p.Provider.Connector(), p.Slot, nil, &p.Provider, p.Model); err != nil {
		if ctx.Err() != nil {
			return nil, exchange.fail(ClassCancelled, nil)
		}
		return nil, exchange.fail(ClassCredential, nil)
	}
	for name, values := range headers {
		req.Header.Del(name)
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	return s.doPinnedRequest(ctx, p, exchange, req, openai.ParseErrorBody)
}

func GeminiFileID(name string) (string, error) {
	prefix, id, ok := strings.Cut(name, "/")
	if !ok || prefix != "files" || id == "" || strings.Contains(id, "/") {
		return "", fmt.Errorf("invalid Gemini file name")
	}
	return id, nil
}

func ValidGeminiFileID(value string) bool {
	const prefix = resources.KindGeminiFile + "_"
	raw := strings.TrimPrefix(value, prefix)
	if raw == value || len(raw) != 32 {
		return false
	}
	id, err := uuid.Parse(raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:])
	return err == nil && resources.LocalID(resources.KindGeminiFile, id) == value
}
