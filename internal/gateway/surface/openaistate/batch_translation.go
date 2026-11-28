package openaistate

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/routing"
)

type translatedBatch struct {
	manifest managedbatch.Manifest
	err      error
}

func (s surface) translateManagedBatchInput(ctx context.Context, p *kernel.Pin, route *routing.Route, file *staging.Part, adapter connectors.BatchAdapter) (*staging.Part, managedbatch.Manifest, *fault.Error) {
	opened, err := s.spool.Open(file.Handle)
	if err != nil {
		return nil, managedbatch.Manifest{}, fault.ServerError(http.StatusInternalServerError, "media_content_unavailable", "The uploaded batch file could not be read.")
	}
	defer func() { _ = opened.File.Close() }()
	reader, writer := io.Pipe()
	done := make(chan translatedBatch, 1)
	go func() {
		defer panics.Recover(s.kernel.Log, "managed batch translation", func(err error) {
			_ = writer.CloseWithError(err)
			done <- translatedBatch{err: fault.ServerError(http.StatusInternalServerError, "internal_error", "The batch file could not be translated.")}
		})
		target := managedbatch.LineTarget{
			Provider: p.Provider, Model: p.Model, Route: route, Adapter: adapter,
			CheckOpenAIEndpoint: func(endpoint, pointer string) *fault.Error {
				return batchEndpointRefusal(p, endpoint, pointer)
			},
		}
		manifest, err := managedbatch.TranslateLines(writer, opened.File, s.lineDigest, target)
		done <- translatedBatch{manifest: manifest, err: err}
	}()
	artifact, spoolErr := s.spool.Put(ctx, staging.Upload{
		Filename: file.Filename, ContentType: "application/jsonl", MaximumLength: file.Limit, Body: reader,
	})
	_ = reader.Close()
	translated := <-done
	if spoolErr != nil || translated.err != nil {
		if spoolErr == nil {
			_ = s.spool.Remove(artifact.Handle)
		}
		if failure, ok := errors.AsType[*fault.Error](translated.err); ok {
			return nil, managedbatch.Manifest{}, failure
		}
		return nil, managedbatch.Manifest{}, fault.InvalidRequest("unsupported_parameter", "The batch file could not be translated.", new("file"))
	}
	part := &staging.Part{
		Handle: artifact.Handle, Digest: artifact.Digest, Filename: file.Filename,
		ContentType: artifact.ContentType, Size: artifact.ContentLength, Limit: file.Limit,
	}
	return part, translated.manifest, nil
}
