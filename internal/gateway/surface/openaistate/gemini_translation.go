package openaistate

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

func (s surface) translatedGeminiResults(ctx context.Context, routeSlug, role string, source *staging.Part, exchange *kernel.PinnedExchange) (*staging.Part, *fault.Error) {
	part, err := s.translateGeminiManagedContent(ctx, routeSlug, source, role)
	if err == nil {
		return part, nil
	}
	exchange.Interrupt(kernel.ClassProtocol, !errors.Is(err, managedbatch.ErrGeminiBatchFidelity))
	if _, tooLarge := errors.AsType[*staging.TooLargeError](err); tooLarge {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The translated Gemini batch file exceeded the response limit.")
	}
	return nil, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini batch results could not be translated.")
}

func (s surface) translateGeminiManagedContent(ctx context.Context, routeSlug string, source *staging.Part, role string) (*staging.Part, error) {
	if role != "output" && role != "error" {
		return nil, managedbatch.ErrGeminiBatchFidelity
	}
	opened, err := s.spool.Open(source.Handle)
	if err != nil {
		return nil, err
	}
	defer func() { _ = opened.File.Close() }()
	reader, writer := io.Pipe()
	limit := min(s.kernel.Config.MaxResponseBytes, source.Limit, batches.MaxBatchBytes)
	done := make(chan error, 1)
	go func() {
		defer panics.Recover(s.kernel.Log, "gemini batch results", func(err error) {
			done <- err
			_ = writer.CloseWithError(err)
		})
		streamErr := managedbatch.TranslateGeminiStream(opened.File, writer, routeSlug, role)
		done <- streamErr
		_ = writer.CloseWithError(streamErr)
	}()
	artifact, spoolErr := s.spool.Put(ctx, staging.Upload{
		Filename: source.Filename, ContentType: "application/jsonl", MaximumLength: limit, Body: reader,
	})
	_ = reader.Close()
	translateErr := <-done
	if spoolErr != nil || translateErr != nil {
		if spoolErr == nil {
			_ = s.spool.Remove(artifact.Handle)
		}
		if spoolErr != nil {
			return nil, spoolErr
		}
		return nil, translateErr
	}
	return &staging.Part{Handle: artifact.Handle, Filename: source.Filename, ContentType: artifact.ContentType, Size: artifact.ContentLength, Limit: limit}, nil
}
