package vendor

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/antigravityauth"
	"github.com/tyk-swe/olp/internal/claudeauth"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/plugin/abi"
)

var ErrImportUnsupported = errors.New("subscription profile does not support credential import")

type ImportedCredential struct {
	Refresh abi.GrantRefresh
	Secrets []string
	Matches func(abi.Grant) bool
}

func PrepareImport(ctx context.Context, adapter code.Adapter, input, clientID string, client *http.Client, now time.Time) (ImportedCredential, error) {
	switch adapter {
	case code.AdapterCodex:
		credentials, err := codexauth.ParseCredentialImport(input, now)
		if err != nil {
			return ImportedCredential{}, err
		}
		return ImportedCredential{Refresh: credentials.RefreshRequest(), Secrets: credentials.Secrets(), Matches: credentials.Matches}, nil
	case code.AdapterClaude:
		credentials, err := claudeauth.ParseCredentialImport(input, now)
		if err != nil {
			return ImportedCredential{}, err
		}
		refresh, err := credentials.Verify(ctx, client)
		if err != nil {
			return ImportedCredential{}, err
		}
		return ImportedCredential{Refresh: refresh, Secrets: credentials.Secrets(), Matches: func(grant abi.Grant) bool {
			return grant.Principal == claudeauth.Principal(refresh.Facts) && maps.Equal(grant.Facts, refresh.Facts)
		}}, nil
	case code.AdapterAntigravity:
		credentials, err := antigravityauth.ParseCredentialImport(input, clientID, now)
		if err != nil {
			return ImportedCredential{}, err
		}
		refresh, err := credentials.Verify(ctx, client)
		if err != nil {
			return ImportedCredential{}, err
		}
		return ImportedCredential{Refresh: refresh, Secrets: credentials.Secrets(), Matches: func(grant abi.Grant) bool {
			return grant.Principal == antigravityauth.Principal(refresh.Facts["subject"], refresh.Facts["project_id"]) && maps.Equal(grant.Facts, refresh.Facts)
		}}, nil
	default:
		return ImportedCredential{}, ErrImportUnsupported
	}
}
