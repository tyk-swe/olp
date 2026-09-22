package oif

import (
	"encoding/hex"
	"errors"
)

// BlobReference identifies bytes owned by an existing asset/spool/resource
// authority. It is not a URL, a store, or permission to fetch arbitrary data.
type BlobReference struct {
	id, digest, mediaType string
	size                  int64
}

func NewBlobReference(id, digest, mediaType string, size int64) (BlobReference, error) {
	hash, err := hex.DecodeString(digest)
	if id == "" || mediaType == "" || size < 0 || err != nil || len(hash) != 32 {
		return BlobReference{}, errors.New("blob reference requires identity, SHA-256, media type and nonnegative size")
	}
	return BlobReference{id, digest, mediaType, size}, nil
}
func (b BlobReference) ID() string        { return b.id }
func (b BlobReference) Digest() string    { return b.digest }
func (b BlobReference) MediaType() string { return b.mediaType }
func (b BlobReference) Size() int64       { return b.size }
func (b BlobReference) Valid() bool       { return b.id != "" }

// BlobRequest uses the same neutral envelope for operation-owned binary or
// multipart runners. Existing owners retain read access and lifecycle policy.
type BlobRequest struct {
	descriptor Descriptor
	source     BlobReference
}

func NewBlobRequest(d Descriptor, source BlobReference) (BlobRequest, error) {
	if !source.Valid() || d.Operation.ID == "" || d.Dialect.ID == "" {
		return BlobRequest{}, errors.New("blob request requires source and operation/dialect identities")
	}
	return BlobRequest{d.clone(), source}, nil
}
func (r BlobRequest) Descriptor() Descriptor { return r.descriptor.clone() }
func (r BlobRequest) Source() BlobReference  { return r.source }

type BlobResult struct {
	descriptor Descriptor
	source     BlobReference
	outcome    Outcome
}

func NewBlobResult(d Descriptor, source BlobReference, outcome Outcome) (BlobResult, error) {
	if !source.Valid() {
		return BlobResult{}, errors.New("blob result requires source")
	}
	return BlobResult{d.clone(), source, outcome}, nil
}
func (r BlobResult) Descriptor() Descriptor { return r.descriptor.clone() }
func (r BlobResult) Source() BlobReference  { return r.source }
func (r BlobResult) Outcome() Outcome       { return r.outcome }
