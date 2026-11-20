package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/staging"
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

func (s *Server) UploadGeminiManagedFile(ctx context.Context, x *Call, p *Pin, part *staging.Part) ([]byte, string, *PinnedExchange, *AttemptFailure) {
	endpoint, err := GeminiUploadOrigin(p.Provider.Connector(), p.Model, s.Egress)
	if err != nil {
		return nil, "", nil, geminiManagedUploadFailure(ClassConnect, false)
	}
	startHeaders := http.Header{
		"X-Goog-Upload-Protocol":              {"resumable"},
		"X-Goog-Upload-Command":               {"start"},
		"X-Goog-Upload-Header-Content-Length": {strconv.FormatInt(part.Size, 10)},
		"X-Goog-Upload-Header-Content-Type":   {"application/jsonl"},
	}
	displayName, _ := json.Marshal(map[string]any{"file": map[string]string{"display_name": part.Filename}})
	start, failure := s.GeminiPinnedRequest(ctx, x, p, http.MethodPost, endpoint, bytes.NewReader(displayName), int64(len(displayName)), "application/json", "application/json", startHeaders)
	if failure != nil {
		return nil, "", nil, failure
	}
	uploadURL := start.Header.Get("X-Goog-Upload-URL")
	generationURL, _ := p.Provider.Connector().URL(wire.Gemini, p.Model, false)
	if len(start.Header.Values("X-Goog-Upload-URL")) != 1 ||
		ValidateGeminiUploadURL(uploadURL, generationURL, s.Egress) != nil {
		_ = start.Body.Close()
		return nil, "", nil, geminiManagedUploadFailure(ClassProtocol, true)
	}
	_ = start.Body.Close()
	start.Exchange.Complete(ClassSuccess)
	opened, err := s.Deps.Staging.Open(part.Handle)
	if err != nil {
		return nil, "", nil, geminiManagedUploadFailure(ClassProtocol, true)
	}
	defer opened.File.Close()
	chunkHeaders := http.Header{
		"X-Goog-Upload-Command": {"upload, finalize"},
		"X-Goog-Upload-Offset":  {"0"},
	}
	final, failure := s.GeminiPinnedRequest(ctx, x, p, http.MethodPost, uploadURL, PinnedUploadSource{opened.File}, part.Size,
		"application/jsonl", "application/json", chunkHeaders)
	if failure != nil {
		return nil, "", nil, failure
	}
	defer final.Body.Close()
	if final.Header.Get("X-Goog-Upload-Status") != "final" {
		return nil, "", nil, geminiManagedUploadFailure(ClassProtocol, true)
	}
	body, err := final.ReadBody(s.Config.MaxResponseBytes)
	if err != nil {
		return nil, "", nil, geminiManagedUploadFailure(ClassProtocol, true)
	}
	var result struct {
		File json.RawMessage `json:"file"`
	}
	var FileObject map[string]json.RawMessage
	if json.Unmarshal(body, &result) != nil || len(result.File) == 0 ||
		json.Unmarshal(result.File, &FileObject) != nil {
		return nil, "", nil, geminiManagedUploadFailure(ClassProtocol, true)
	}
	var name string
	if json.Unmarshal(FileObject["name"], &name) != nil {
		return nil, "", nil, geminiManagedUploadFailure(ClassProtocol, true)
	}
	if _, err := GeminiFileID(name); err != nil {
		return nil, "", nil, geminiManagedUploadFailure(ClassProtocol, true)
	}
	return body, name, final.Exchange, nil
}

func geminiManagedUploadFailure(class string, dispatched bool) *AttemptFailure {
	return &AttemptFailure{class: class, Status: http.StatusBadGateway, dispatched: dispatched}
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
