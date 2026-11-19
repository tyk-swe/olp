package access

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
)

const settingJSON = `to_jsonb(s)||CASE WHEN s.key='budgets.time_zone' THEN jsonb_build_object('calendar',olp.budget_calendar_status(now())) ELSE '{}'::jsonb END`

func (s *Server) settings(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	var err error
	rows, err := s.Pool.Query(r.Context(), "SELECT "+settingJSON+" FROM olp.settings s ORDER BY key")
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	return httpkit.OK(map[string]any{"items": items}), err
}

func (s *Server) setting(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	var err error
	var data []byte
	var etag string
	err = s.Pool.QueryRow(r.Context(), "SELECT "+settingJSON+",etag::text FROM olp.settings s WHERE key=$1", r.PathValue("key")).Scan(&data, &etag)
	return httpkit.Detail(json.RawMessage(data), etag), err
}

func (s *Server) updateSetting(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input struct {
		Value string `json:"value"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	key := r.PathValue("key")
	switch key {
	case "retention.requests_days", "retention.usage_days", "retention.audit_days":
		n, err := strconv.Atoi(input.Value)
		if err != nil || n < 1 || n > 3650 {
			return httpkit.Reply{}, httpkit.Invalid("value", "Use an integer from 1 to 3650.")
		}
	case "guardrails.stream_window_bytes":
		n, err := strconv.Atoi(input.Value)
		if err != nil || n < 256 || n > 1<<20 {
			return httpkit.Reply{}, httpkit.Invalid("value", "Use a byte window between 256 and 1048576.")
		}
	case "budgets.time_zone":
		if err := ValidateBudgetTimeZone(input.Value); err != nil {
			return httpkit.Reply{}, httpkit.Invalid("value", "Use a supported IANA time zone.")
		}
	case "limits.valkey_unavailable":
		if input.Value != "fail_open" && input.Value != "fail_closed" {
			return httpkit.Reply{}, httpkit.Invalid("value", "Use fail_open or fail_closed.")
		}
	case "auth.local_login_enabled", "auth.mfa_required":
		if input.Value != "true" && input.Value != "false" {
			return httpkit.Reply{}, httpkit.Invalid("value", "Use true or false.")
		}
	default:
		return httpkit.Reply{}, httpkit.Fail(404, "not_found", "This setting does not exist.")
	}
	return Transact(s, r, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		return struct{}{}, settingChangeable(r, tx, p, key)
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, Event, error) {
		reply, err := s.storeSetting(r, tx, p, key, input.Value)
		return reply, Event{Action: "setting.update", Resource: "setting", ID: key}, err
	})
}

func settingChangeable(r *http.Request, tx pgx.Tx, p authz.Principal, key string) error {
	needs := map[string]authz.Operation{"auth.local_login_enabled": authz.LocalLogin, "auth.mfa_required": authz.Access}
	if operation, restricted := needs[key]; restricted {
		if err := p.Authorize(operation); err != nil {
			return err
		}
	}
	var etag string
	if err := tx.QueryRow(r.Context(), "SELECT etag::text FROM olp.settings WHERE key=$1", key).Scan(&etag); err != nil {
		return err
	}
	return httpkit.Match(r, etag)
}

func (s *Server) storeSetting(r *http.Request, tx pgx.Tx, p authz.Principal, key, value string) (httpkit.Reply, error) {
	if key == "budgets.time_zone" {
		if _, err := tx.Exec(r.Context(), "SELECT olp.schedule_budget_zone($1,now())", value); err != nil {
			return httpkit.Reply{}, err
		}
	}
	etag := httpkit.NewID()
	if _, err := tx.Exec(r.Context(), "UPDATE olp.settings SET value=$1,etag=$2,updated_by=$3,updated_at=now() WHERE key=$4", value, etag, p.UserID(), key); err != nil {
		return httpkit.Reply{}, err
	}
	if key == "auth.local_login_enabled" {
		if err := s.EnsureUsableOwner(r.Context(), tx); err != nil {
			return httpkit.Reply{}, err
		}
	}
	if key == "guardrails.stream_window_bytes" && s.RuntimePublisher != nil {
		if err := s.RuntimePublisher(r.Context(), tx, p.UserID()); err != nil {
			return httpkit.Reply{}, httpkit.Invalid("value", "The window is incompatible with published guardrails.")
		}
	}
	var data []byte
	err := tx.QueryRow(r.Context(), "SELECT "+settingJSON+" FROM olp.settings s WHERE key=$1", key).Scan(&data)
	return httpkit.Detail(json.RawMessage(data), etag), err
}
