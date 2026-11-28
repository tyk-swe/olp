package providers

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/antigravityauth"
	"github.com/tyk-swe/olp/internal/codemode/claudeauth"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode/codexauth"
	"github.com/tyk-swe/olp/internal/codesessions/codeaccounts"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/plugin/abi"
)

type credentialImportInput struct {
	Credentials string `json:"credentials"`
	SlotID      string `json:"slot_id"`
}

type credentialImportSlot struct {
	ID           string
	CredentialID *string
	Principal    string
}

func (s *enrollmentService) importCredentials(adapter codemode.Adapter) access.Handler {
	return func(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
		return s.importCredentialsAs(r, p, adapter)
	}
}

func (s *enrollmentService) importCredentialsAs(r *http.Request, p authz.Principal, adapter codemode.Adapter) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input credentialImportInput
	if err := httpkit.DecodeUnique(r, &input, 128<<10); err != nil {
		return httpkit.Reply{}, err
	}
	current, err := readProvider(r.Context(), s.access.Pool, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(current.ProjectID, authz.Change); err != nil {
		return httpkit.Reply{}, err
	}
	if err := httpkit.Match(r, current.ETag); err != nil {
		return httpkit.Reply{}, err
	}
	cfg := current.Connection.Document()
	vendor, supported := codeadapter.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID)
	if !supported || vendor.Adapter != adapter {
		return httpkit.Reply{}, problem.Invalid("credentials", "This connection does not use the selected subscription profile.")
	}
	served, err := cfg.Serving()
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := codeaccounts.ValidateConnection(r.Context(), s.plugins, served, vendor); err != nil {
		return httpkit.Reply{}, problem.Invalid("credentials", "This connection must use the unmodified subscription plugin and configuration.")
	}
	slot, err := s.credentialImportSlot(r, id, input.SlotID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	client, err := s.prober.Client(r.Context(), current.target(nil))
	if err != nil {
		return httpkit.Reply{}, err
	}
	credentials, err := prepareCredentialImport(r.Context(), adapter, input.Credentials, cfg.Options.PluginOptions["client_id"], client)
	if err != nil {
		return httpkit.Reply{}, problem.Invalid("credentials", "Could not verify the supported credential format and original account. Sign in again and retry.")
	}
	grant, err := s.plugins.RefreshGrant(r.Context(), cfg.ProfileRevision, abi.Provider{Profile: cfg.ProfileID, Options: cfg.Options.PluginOptions}, credentials.refresh, client, credentials.secrets)
	if err != nil || grants.ValidateTokenMaterial(grant.AccessToken, grant.RefreshToken, grant.ExpiresIn) != nil || !credentials.matches(grant) || slot.CredentialID != nil && slot.Principal != grant.Principal {
		if auditErr := s.auditFailedEnrollment(r, id); auditErr != nil {
			return httpkit.Reply{}, auditErr
		}
		return httpkit.Reply{}, problem.Invalid("credentials", "Could not verify the imported credentials for this account. Sign in again and retry.")
	}
	verified := abi.Grant{AccessToken: grant.AccessToken, RefreshToken: cmp.Or(grant.RefreshToken, credentials.refresh.RefreshToken),
		ExpiresIn: grant.ExpiresIn, Principal: grant.Principal, Facts: grant.Facts}
	return s.stageCredentialImport(r, current, slot, verified, adapter)
}

func (s *enrollmentService) credentialImportSlot(r *http.Request, providerID, slotID string) (credentialImportSlot, error) {
	identifier, parseErr := uuid.Parse(slotID)
	if slotID != "" && parseErr != nil {
		return credentialImportSlot{}, problem.Invalid("slot_id", "Use a credential slot identifier.")
	}
	var slot credentialImportSlot
	err := s.access.Pool.QueryRow(r.Context(), `SELECT s.id::text,s.credential_id::text,coalesce(c.principal,'')
		FROM olp.provider_slots s LEFT JOIN olp.provider_credentials c ON c.id=s.credential_id
		WHERE s.provider_id=$1 AND (($3 AND s.is_default) OR (NOT $3 AND s.id=$2))`, providerID, identifier, slotID == "").Scan(&slot.ID, &slot.CredentialID, &slot.Principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialImportSlot{}, problem.Invalid("slot_id", "Unknown credential slot for this connection.")
	}
	return slot, err
}

func (s *enrollmentService) stageCredentialImport(request *http.Request, current *record, slot credentialImportSlot, grant abi.Grant, adapter codemode.Adapter) (httpkit.Reply, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), stageGrantTimeout)
	defer cancel()
	r := request.WithContext(ctx)
	return access.Transact(s.access, r, changeable(ctx, current.ID), func(tx pgx.Tx, _ authz.Principal, locked *record) (httpkit.Reply, access.Event, error) {
		if err := httpkit.Match(r, locked.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		credentialID, version, err := grants.Store(ctx, tx, s.access, current.ID, current.Connection.Document().ProfileRevision, current.Connection.Document().ProfileID, grant)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		completed, err := bindGrant(ctx, tx, grantBinding{ProviderID: current.ID, SlotID: slot.ID, ExpectedCredentialID: slot.CredentialID,
			Credential: grants.Credential{ID: credentialID, Version: version, Principal: grant.Principal}},
			problem.Fail(409, string(adapter)+"_import_stale", "The credential slot changed during import. Reload before retrying."))
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: 201, ETag: completed.ETag, Body: completed}, access.Event{Action: "provider.grant.import", Resource: "provider_credential", ID: completed.CredentialID}, nil
	})
}

type importedCredential struct {
	refresh abi.GrantRefresh
	secrets []string
	matches func(abi.Grant) bool
}

func prepareCredentialImport(ctx context.Context, adapter codemode.Adapter, input, clientID string, client *http.Client) (importedCredential, error) {
	switch adapter {
	case codemode.AdapterCodex:
		credentials, err := codexauth.ParseCredentialImport(input, time.Now())
		if err != nil {
			return importedCredential{}, err
		}
		return importedCredential{refresh: credentials.RefreshRequest(), secrets: credentials.Secrets(), matches: credentials.Matches}, nil
	case codemode.AdapterClaude:
		credentials, err := claudeauth.ParseCredentialImport(input, time.Now())
		if err != nil {
			return importedCredential{}, err
		}
		refresh, err := credentials.Verify(ctx, client)
		if err != nil {
			return importedCredential{}, err
		}
		return importedCredential{refresh: refresh, secrets: credentials.Secrets(), matches: func(grant abi.Grant) bool {
			return grant.Principal == claudeauth.Principal(refresh.Facts) && maps.Equal(grant.Facts, refresh.Facts)
		}}, nil
	case codemode.AdapterAntigravity:
		credentials, err := antigravityauth.ParseCredentialImport(input, clientID, time.Now())
		if err != nil {
			return importedCredential{}, err
		}
		refresh, err := credentials.Verify(ctx, client)
		if err != nil {
			return importedCredential{}, err
		}
		return importedCredential{refresh: refresh, secrets: credentials.Secrets(), matches: func(grant abi.Grant) bool {
			return grant.Principal == antigravityauth.Principal(refresh.Facts["subject"], refresh.Facts["project_id"]) && maps.Equal(grant.Facts, refresh.Facts)
		}}, nil
	default:
		return importedCredential{}, problem.Invalid("credentials", "This subscription profile does not support credential import.")
	}
}
