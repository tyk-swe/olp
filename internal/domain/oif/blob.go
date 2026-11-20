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
