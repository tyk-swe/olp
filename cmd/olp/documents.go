package main

import (
	"log/slog"

	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/pluginindex"
	"github.com/tyk-swe/olp/internal/signing"
)

type signedDocuments struct {
	catalogKeys signing.Keyring
	catalog     *catalog.Signed
	pluginIndex *pluginindex.Signed
}

func loadSignedDocuments(log *slog.Logger) (signedDocuments, error) {
	// The signed documents a release ships verify before anything starts: a
	// tampered reference catalog must never feed discovery, prices or token
	// estimates.
	catalogKeys, err := signing.Trusted()
	if err != nil {
		return signedDocuments{}, err
	}
	referenceCatalog, err := catalog.Embedded()
	if err != nil {
		return signedDocuments{}, err
	}
	log.Info("reference catalog verified", "sha256", referenceCatalog.SHA256, "key_id", referenceCatalog.KeyID, "published_at", referenceCatalog.Catalog.PublishedAt)
	pluginIndex, err := pluginindex.Embedded()
	if err != nil {
		return signedDocuments{}, err
	}
	log.Info("plugin index verified", "sha256", pluginIndex.SHA256, "key_id", pluginIndex.KeyID, "published_at", pluginIndex.Index.PublishedAt)
	return signedDocuments{catalogKeys: catalogKeys, catalog: referenceCatalog, pluginIndex: pluginIndex}, nil
}
