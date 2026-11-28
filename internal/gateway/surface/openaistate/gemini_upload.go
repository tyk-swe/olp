package openaistate

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

func incompleteProviderUpload() *fault.Error {
	return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider could not complete the request.")
}

func (s surface) uploadGeminiManagedFile(ctx context.Context, x *kernel.Call, p *kernel.Pin, part *staging.Part) (providerFile, *fault.Error) {
	uploadURL, e := s.startGeminiUpload(ctx, x, p, part)
	if e != nil {
		return providerFile{}, e
	}
	return s.finishGeminiUpload(ctx, x, p, part, uploadURL)
}

func (s surface) startGeminiUpload(ctx context.Context, x *kernel.Call, p *kernel.Pin, part *staging.Part) (string, *fault.Error) {
	endpoint, err := kernel.GeminiUploadOrigin(p.Provider.Connector(), p.Model, s.kernel.Egress)
	if err != nil {
		return "", incompleteProviderUpload()
	}
	startHeaders := http.Header{
		"X-Goog-Upload-Protocol":              {"resumable"},
		"X-Goog-Upload-Command":               {"start"},
		"X-Goog-Upload-Header-Content-Length": {strconv.FormatInt(part.Size, 10)},
		"X-Goog-Upload-Header-Content-Type":   {"application/jsonl"},
	}
	displayName, _ := json.Marshal(map[string]any{"file": map[string]string{"display_name": part.Filename}})
	start, failure := s.kernel.GeminiPinnedRequest(ctx, x, p, http.MethodPost, endpoint, bytes.NewReader(displayName), int64(len(displayName)), "application/json", "application/json", startHeaders)
	if failure != nil {
		return "", kernel.UpstreamError(failure)
	}
	uploadURL := start.Header.Get("X-Goog-Upload-URL")
	generationURL, _ := p.Provider.Connector().URL(wire.Gemini, p.Model, false)
	if len(start.Header.Values("X-Goog-Upload-URL")) != 1 ||
		kernel.ValidateGeminiUploadURL(uploadURL, generationURL, s.kernel.Egress) != nil {
		_ = start.Body.Close()
		return "", incompleteProviderUpload()
	}
	_ = start.Body.Close()
	start.Exchange.Complete(kernel.ClassSuccess)
	return uploadURL, nil
}

func (s surface) finishGeminiUpload(ctx context.Context, x *kernel.Call, p *kernel.Pin, part *staging.Part, uploadURL string) (providerFile, *fault.Error) {
	opened, err := s.spool.Open(part.Handle)
	if err != nil {
		return providerFile{}, incompleteProviderUpload()
	}
	defer func() { _ = opened.File.Close() }()
	chunkHeaders := http.Header{
		"X-Goog-Upload-Command": {"upload, finalize"},
		"X-Goog-Upload-Offset":  {"0"},
	}
	final, failure := s.kernel.GeminiPinnedRequest(ctx, x, p, http.MethodPost, uploadURL, kernel.PinnedUploadSource{Reader: opened.File}, part.Size,
		"application/jsonl", "application/json", chunkHeaders)
	if failure != nil {
		return providerFile{}, kernel.UpstreamError(failure)
	}
	defer func() { _ = final.Body.Close() }()
	if final.Header.Get("X-Goog-Upload-Status") != "final" {
		return providerFile{}, incompleteProviderUpload()
	}
	body, err := final.ReadBody(s.kernel.Config.MaxResponseBytes)
	if err != nil {
		return providerFile{}, incompleteProviderUpload()
	}
	name, ok := uploadedGeminiFileName(body)
	if !ok {
		return providerFile{}, incompleteProviderUpload()
	}
	return providerFile{body: body, upstreamID: name, exchange: final.Exchange}, nil
}

func uploadedGeminiFileName(body []byte) (string, bool) {
	var result struct {
		File json.RawMessage `json:"file"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &result) != nil || len(result.File) == 0 ||
		json.Unmarshal(result.File, &fields) != nil {
		return "", false
	}
	var name string
	if json.Unmarshal(fields["name"], &name) != nil {
		return "", false
	}
	if _, err := kernel.GeminiFileID(name); err != nil {
		return "", false
	}
	return name, true
}
