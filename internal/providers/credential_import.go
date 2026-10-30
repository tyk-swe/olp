package providers

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/claudeauth"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
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

func (s *Server) importCodexCredentials(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.importCredentials(r, p, codemode.AdapterCodex)
}

func (s *Server) importClaudeCredentials(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.importCredentials(r, p, codemode.AdapterClaude)
}

func (s *Server) importCredentials(r *http.Request, p access.Principal, adapter codemode.Adapter) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input credentialImportInput
	if err := access.DecodeUnique(r, &input, 128<<10); err != nil {
		return access.Reply{}, err
	}
	current, err := load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	cfg := current.Configuration
	vendor, supported := codeadapter.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID)
	if !supported || vendor.Adapter != adapter {
		return access.Reply{}, access.Invalid("credentials", "This connection does not use the selected subscription profile.")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return access.Reply{}, err
	}
	var connection routing.Configuration
	if err := json.Unmarshal(encoded, &connection); err != nil {
		return access.Reply{}, err
	}
	if err := codeConnection(r.Context(), s.Plugins, connection, vendor); err != nil {
		return access.Reply{}, access.Invalid("credentials", "This connection must use the unmodified subscription plugin and configuration.")
	}
	slot, err := s.credentialImportSlot(r, id, input.SlotID)
	if err != nil {
		return access.Reply{}, err
	}
	client, err := s.connectionClient(r.Context(), &cfg, nil)
	if err != nil {
		return access.Reply{}, err
	}
	credentials, err := prepareCredentialImport(r.Context(), adapter, input.Credentials, client)
	if err != nil {
		return access.Reply{}, access.Invalid("credentials", "Could not verify the supported credential format and original account. Sign in again and retry.")
	}
	grant, err := s.Plugins.RefreshGrant(r.Context(), cfg.ProfileRevision, abi.Provider{Profile: cfg.ProfileID, Options: cfg.Options.PluginOptions}, credentials.refresh, client, credentials.secrets)
	if err != nil || grants.ValidateTokenMaterial(grant.AccessToken, grant.RefreshToken, grant.ExpiresIn) != nil || !credentials.matches(grant) || slot.CredentialID != nil && slot.Principal != grant.Principal {
		if auditErr := s.auditFailedGrantEnrollment(r, p.Actor(), id); auditErr != nil {
			return access.Reply{}, auditErr
		}
		return access.Reply{}, access.Invalid("credentials", "Could not verify the imported credentials for this account. Sign in again and retry.")
	}
	verified := abi.Grant{AccessToken: grant.AccessToken, RefreshToken: cmp.Or(grant.RefreshToken, credentials.refresh.RefreshToken),
		ExpiresIn: grant.ExpiresIn, Principal: grant.Principal, Facts: grant.Facts}
	return s.stageCredentialImport(r, current, slot, verified, adapter)
}

func (s *Server) credentialImportSlot(r *http.Request, providerID, slotID string) (credentialImportSlot, error) {
	identifier, parseErr := uuid.Parse(slotID)
	if slotID != "" && parseErr != nil {
		return credentialImportSlot{}, access.Invalid("slot_id", "Use a credential slot identifier.")
	}
	var slot credentialImportSlot
	err := s.Access.Pool.QueryRow(r.Context(), `SELECT s.id::text,s.credential_id::text,coalesce(c.principal,'')
		FROM olp.provider_slots s LEFT JOIN olp.provider_credentials c ON c.id=s.credential_id
		WHERE s.provider_id=$1 AND (($3 AND s.is_default) OR (NOT $3 AND s.id=$2))`, providerID, identifier, slotID == "").Scan(&slot.ID, &slot.CredentialID, &slot.Principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialImportSlot{}, access.Invalid("slot_id", "Unknown credential slot for this connection.")
	}
	return slot, err
}

func (s *Server) stageCredentialImport(request *http.Request, current *record, slot credentialImportSlot, grant abi.Grant, adapter codemode.Adapter) (access.Reply, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), stageGrantTimeout)
	defer cancel()
	r := request.WithContext(ctx)
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(ctx)
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	locked, err := load(ctx, tx, current.ID, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(locked.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, locked.ETag); err != nil {
		return access.Reply{}, err
	}
	credentialID, version, err := grants.Store(ctx, tx, s.Access, current.ID, current.Configuration.ProfileRevision, current.Configuration.ProfileID, grant)
	if err != nil {
		return access.Reply{}, err
	}
	return s.bindGrant(r, tx, p.Actor(), grantBinding{ProviderID: current.ID, SlotID: slot.ID, ExpectedCredentialID: slot.CredentialID,
		Credential: grants.Credential{ID: credentialID, Version: version, Principal: grant.Principal}, Action: "provider.grant.import"},
		access.Fail(409, string(adapter)+"_import_stale", "The credential slot changed during import. Reload before retrying."))
}

type importedCredential struct {
	refresh abi.GrantRefresh
	secrets []string
	matches func(abi.Grant) bool
}

func prepareCredentialImport(ctx context.Context, adapter codemode.Adapter, input string, client *http.Client) (importedCredential, error) {
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
	default:
		return importedCredential{}, access.Invalid("credentials", "This subscription profile does not support credential import.")
	}
}
