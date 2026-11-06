package publications

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
)

type draftRequest struct {
	agentcontract.Draft
	Credential        json.RawMessage `json:"credential,omitempty"`
	EntraClientSecret *string         `json:"entra_client_secret,omitempty"`
}
type labelRequest struct {
	RevisionID string `json:"revision_id"`
}
type change struct {
	draft draftRequest
	label labelRequest
	input json.RawMessage
}

func decodeChange(r *http.Request, kind, action string) (change, error) {
	switch action {
	case "create", "update":
		var request draftRequest
		if err := access.DecodeUnique(r, &request, 1<<20); err != nil {
			return change{}, err
		}
		if err := request.Draft.Validate(kind); err != nil {
			return change{}, access.Invalid("draft", err.Error())
		}
		input, err := json.Marshal(request)
		return change{draft: request, input: input}, err
	case "label":
		var label labelRequest
		if err := access.DecodeUnique(r, &label, 1024); err != nil {
			return change{}, err
		}
		input, err := json.Marshal(label)
		return change{label: label, input: input}, err
	default:
		return change{input: json.RawMessage(`{}`)}, nil
	}
}
func (s *Server) current(r *http.Request, tx pgx.Tx, p access.Principal, kind, action string, input change) (*agentcontract.Publication, error) {
	if action == "create" {
		return nil, s.access.RequireProject(r.Context(), tx, p, &input.draft.ProjectID)
	}
	id, err := access.IDParam(r, "publication_id")
	if err != nil {
		return nil, err
	}
	current, loadErr := Load(r.Context(), tx, id, kind, true)
	if loadErr != nil {
		return nil, mapConflict(loadErr)
	}
	if err := p.Project(&current.ProjectID, access.Change); err != nil {
		return nil, err
	}
	return current, nil
}
func (s *Server) apply(r *http.Request, tx pgx.Tx, p access.Principal, current *agentcontract.Publication, kind, action string, input change) (agentcontract.Publication, error) {
	switch action {
	case "create", "update":
		credentialID, err := s.credential(r.Context(), tx, current, input.draft.Draft, input.draft.Credential, input.draft.EntraClientSecret)
		if err != nil {
			return agentcontract.Publication{}, err
		}
		draft := draftWithCredential(input.draft.Draft, credentialID)
		if err := AuthorizeTrustChange(p, current, draft); err != nil {
			return agentcontract.Publication{}, err
		}
		return SaveDraft(r.Context(), tx, p.UserID(), current, kind, draft, s.egress)
	case "publish":
		next, err := Publish(r.Context(), tx, p.UserID(), *current)
		if err != nil {
			return agentcontract.Publication{}, err
		}
		if next.Kind == "agent" && (s.validateAgent == nil || s.validateAgent(r.Context(), *next.Draft.Agent) != nil) {
			return agentcontract.Publication{}, access.Invalid("agent", "The pinned agent card must declare the configured protocol and JSON-RPC endpoint.")
		}
		return next, nil
	case "retire", "label":
		next := agentcontract.Publication{ID: current.ID, Kind: current.Kind, ProjectID: current.ProjectID, Name: current.Name, ETag: access.NewID(), Draft: current.Draft, RevisionID: current.RevisionID, Retired: action == "retire"}
		if action == "label" {
			if err := SetLabel(r.Context(), tx, next, r.PathValue("label"), input.label.RevisionID, next.ETag); err != nil {
				return agentcontract.Publication{}, err
			}
		}
		_, err := tx.Exec(r.Context(), `UPDATE olp.agent_publications SET retired=$2,etag=$3 WHERE id=$1`, next.ID, next.Retired, next.ETag)
		return next, err
	default:
		return agentcontract.Publication{}, access.Invalid("action", "Unknown publication action.")
	}
}
func (s *Server) write(r *http.Request, kind, action string) (access.Reply, error) {
	input, err := decodeChange(r, kind, action)
	if err != nil {
		return access.Reply{}, err
	}
	tx, principal, authErr := s.access.BeginMutation(r)
	if authErr != nil {
		return access.Reply{}, authErr
	}
	defer tx.Rollback(r.Context())
	current, loadErr := s.current(r, tx, principal, kind, action, input)
	if loadErr != nil {
		return access.Reply{}, loadErr
	}
	claim, replay, replayErr := s.access.Replay(r, tx, principal, input.input)
	if replayErr != nil {
		return access.Reply{}, replayErr
	}
	if replay != nil {
		return *replay, tx.Commit(r.Context())
	}
	if current != nil {
		if err := access.Match(r, current.ETag); err != nil {
			return access.Reply{}, err
		}
		if current.Retired {
			return access.Reply{}, access.Fail(409, "publication_retired", "Retired history cannot be changed.")
		}
	}
	next, applyErr := s.apply(r, tx, principal, current, kind, action, input)
	if applyErr != nil {
		var pg *pgconn.PgError
		if errors.As(applyErr, &pg) && pg.Code == "23505" {
			return access.Reply{}, access.Fail(409, "publication_exists", "Choose an unused name in this project.")
		}
		return access.Reply{}, applyErr
	}
	if action == "publish" || action == "retire" || action == "label" {
		if _, err := access.AdvanceAuthority(r.Context(), tx); err != nil {
			return access.Reply{}, err
		}
		if s.access.RuntimePublisher != nil {
			if err := s.access.RuntimePublisher(r.Context(), tx, principal.UserID()); err != nil {
				return access.Reply{}, err
			}
		}
	}
	if err := access.AuditForProject(r.Context(), tx, r, principal.Actor(), kind+"."+action, kind, next.ID, "success", &next.ProjectID); err != nil {
		return access.Reply{}, err
	}
	response := publicationReply(next, action)
	if err := s.access.CompleteReplay(r, tx, claim, response); err != nil {
		return access.Reply{}, err
	}
	return response, tx.Commit(r.Context())
}
func publicationReply(publication agentcontract.Publication, action string) access.Reply {
	if action == "retire" {
		return access.Reply{Status: http.StatusNoContent}
	}
	detail := access.Detail(publication, publication.ETag)
	if action == "create" {
		return access.Reply{Status: http.StatusCreated, Body: detail.Body, ETag: detail.ETag, Location: "/api/v1/" + publication.Kind + "s/" + publication.ID}
	}
	return detail
}
