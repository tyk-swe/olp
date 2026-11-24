package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
)

const (
	managedBatchLineLimit = managedbatch.LineLimit
	GeminiNativeManifest  = managedbatch.GeminiNativeManifest
)

var ErrProviderFileMetadata = managedbatch.ErrProviderFileMetadata

type (
	BatchManifest     = managedbatch.Manifest
	managedFileObject = managedbatch.FileObject
	managedFileRecord = managedbatch.FileRecord
)

func DecodeManagedFileRecord(metadata json.RawMessage) (managedFileRecord, error) {
	return managedbatch.DecodeFileRecord(metadata)
}

func ResourceAdapter(resource *resources.Resource) (connectors.BatchAdapter, error) {
	return managedbatch.ResourceAdapter(resource)
}

func ManagedFileMetadata(prior json.RawMessage, response []byte, adapter connectors.BatchAdapter, manifest BatchManifest, role string) (json.RawMessage, error) {
	return managedbatch.FileMetadata(prior, response, adapter, manifest, role)
}

type translatedBatch struct {
	manifest BatchManifest
	err      error
}

func (s *Server) TranslateManagedBatchInput(ctx context.Context, p *Pin, route *routing.Route, file *staging.Part, adapter connectors.BatchAdapter) (*staging.Part, BatchManifest, *fault.Error) {
	opened, err := s.Deps.Staging.Open(file.Handle)
	if err != nil {
		return nil, BatchManifest{}, fault.ServerError(http.StatusInternalServerError, "media_content_unavailable", "The uploaded batch file could not be read.")
	}
	defer opened.File.Close()
	reader, writer := io.Pipe()
	done := make(chan translatedBatch, 1)
	go func() {
		defer panics.Recover(s.Log, "managed batch translation", func(err error) {
			_ = writer.CloseWithError(err)
			done <- translatedBatch{err: fault.ServerError(http.StatusInternalServerError, "internal_error", "The batch file could not be translated.")}
		})
		target := managedbatch.LineTarget{
			Provider: p.Provider, Model: p.Model, Route: route, Adapter: adapter,
			CheckOpenAIEndpoint: func(endpoint, pointer string) *fault.Error {
				return BatchEndpointRefusal(p, endpoint, pointer)
			},
		}
		manifest, err := managedbatch.TranslateLines(writer, opened.File, s.BatchLineDigest, target)
		done <- translatedBatch{manifest: manifest, err: err}
	}()
	artifact, spoolErr := s.Deps.Staging.Put(ctx, staging.Upload{
		Filename: file.Filename, ContentType: "application/jsonl", MaximumLength: file.Limit, Body: reader,
	})
	_ = reader.Close()
	translated := <-done
	if spoolErr != nil || translated.err != nil {
		if spoolErr == nil {
			_ = s.Deps.Staging.Remove(artifact.Handle)
		}
		if failure, ok := errors.AsType[*fault.Error](translated.err); ok {
			return nil, BatchManifest{}, failure
		}
		return nil, BatchManifest{}, fault.InvalidRequest("unsupported_parameter", "The batch file could not be translated.", new("file"))
	}
	part := &staging.Part{
		Handle: artifact.Handle, Digest: artifact.Digest, Filename: file.Filename,
		ContentType: artifact.ContentType, Size: artifact.ContentLength, Limit: file.Limit,
	}
	return part, translated.manifest, nil
}

func (s *Server) BatchLineDigest(customID string) ([]byte, bool) {
	digester := s.Deps.Digester
	if digester == nil {
		return nil, false
	}
	return digester.Digest(secrets.BatchLineDigest, customID), true
}

func ManagedBatchRequestBody(source oif.Document, adapter connectors.BatchAdapter, endpoint string, window BatchWindow, upstreamFileID, model string, manifest BatchManifest, id uuid.UUID) ([]byte, *fault.Error) {
	return managedbatch.RequestBody(source, adapter, endpoint, managedbatch.Window{Label: window.Label, Length: window.Length}, upstreamFileID, model, manifest, id)
}
